package syncer

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"s3sync/splitter"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	_ "github.com/mattn/go-sqlite3"
	"github.com/pterm/pterm"
)

type Syncer struct {
	db         *sql.DB
	FolderPath string
	S3Client   *s3.Client
	Bucket     string
}

// UploadDiffs uploads the files(paths) in the diffs slice, will commit to glacier deep archive if deep is set to true
func (app *Syncer) UploadDiffs(ctx context.Context, diffs []string, deep bool) error {
	count := len(diffs)
	if count == 0 {
		pterm.Success.Println("No files to update!")
		return nil
	}

	for i, v := range diffs {
		spinnerInfo, err := pterm.DefaultSpinner.Start(fmt.Sprintf("Uploading file: %s. %d/%d", v, i+1, count))
		if err != nil {
			return err
		}

		err = app.putObject(ctx, v, spinnerInfo, deep)
		if err != nil {
			spinnerInfo.Fail(err)
			return err
		}
		err = app.updateUploadStatus(v)
		if err != nil {
			spinnerInfo.Fail(err)
			return err
		}
		spinnerInfo.Success(fmt.Sprintf("Successfully uploaded file: %s. %d/%d", v, i+1, count))
	}

	return nil
}

// UpdateManifest Updates the database for all the files (paths) specified in objs slice
func (app *Syncer) UpdateManifest(objs map[string]int64) error {

	for k, v := range objs {
		app.updateRecord(k, v)
	}
	return nil
}

// WalkAndHash walks the directory structure that is specifed in the Syncer.Folderpath.
// Will filter for filetypes listed in the filters slice.
// Returns a map of filepath[lastModDate]
func (app *Syncer) WalkAndHash(filters []string) (map[string]int64, error) {
	spinnerInfo, err := pterm.DefaultSpinner.Start("Taking inventory of existing files.")
	if err != nil {
		return nil, err
	}
	retMap := make(map[string]int64)
	err = filepath.Walk(app.FolderPath, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			if !info.IsDir() {
				if !inFilters(info.Name(), filters) {
					return nil
				}
				h, err := getLastModDate(p)
				if err != nil {
					spinnerInfo.Fail(err)
					return err
				}
				p := app.localize(p)
				retMap[p] = h
			}

		}
		return nil
	})
	if err != nil {
		spinnerInfo.Fail(err)
		return nil, err
	}
	spinnerInfo.Success("Taking Inventory of local files.")
	return retMap, nil
}

// inFilters checks to see if the name of the file has one of the extensions listed in the filters slice, it returns true.
func inFilters(name string, filters []string) bool {
	for _, filter := range filters {
		if strings.HasSuffix(name, filter) {
			return true
		}
	}
	return false
}

// localize converts paths to windows paths if needed, has its own function for future needs.
func (app *Syncer) localize(s string) string {
	s = filepath.FromSlash(s)
	return s

}

// putObject actially performs the uploading to the S3 bucket for the file (path) specified by obj.
// if deep is true, will put it in glacier deep storage.
// Here is where the logic will live that will split files if they are too big
func (app *Syncer) putObject(ctx context.Context, obj string, spinner1 *pterm.SpinnerPrinter, deep bool) error {
	// Lets check the size first, if it is over 5GB ware are going to need to split it.

	info, err := os.Stat(obj)
	if err != nil {
		return err
	}

	if info.Size() > 4294967296 {
		spinner1.Warning(fmt.Sprintf("%s too big for S3, Splitting into multiple files.", obj))
		pieces, err := app.splitObject(obj, info)
		if err != nil {
			return err
		}
		return app.putObjs(ctx, pieces, deep)
	}

	f, err := os.Open(obj)
	if err != nil {
		return err
	}
	defer f.Close()

	storageClass := types.StorageClassStandard
	if deep {
		storageClass = types.StorageClassDeepArchive
	}
	_, err = app.S3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(app.Bucket),
		Key:          aws.String(app.localize(obj)),
		StorageClass: storageClass,
		Body:         f,
	})
	if err != nil {
		return err
	}
	return nil

}

func (app *Syncer) splitObject(obj string, info fs.FileInfo) ([]string, error) {
	id, err := app.setMultipart(obj)
	if err != nil {
		return nil, err
	}

	if err != nil {
		return nil, err
	}

	progress := make(chan string)
	retErr := make(chan error)
	var pieces []string
	count := 0
	go splitter.SplitFile(obj, progress, retErr)
	spinnerInfo, err := pterm.DefaultSpinner.Start(fmt.Sprintf("Splitting %s", obj))
	if err != nil {
		return nil, err
	}
	for {
		select {
		case piece := <-progress:
			pieces = append(pieces, piece)
			count++
			spinnerInfo.UpdateText(fmt.Sprintf("Piece: %s created successfully, now creating piece %d", piece, count))
		case err = <-retErr:
			if err == nil {
				spinnerInfo.Success(fmt.Sprintf("Done splitting. Split %s into %d files", info.Name(), len(pieces)))
				goto End
			}
			spinnerInfo.Fail(err)
			goto End
		}
	}
End:

	defer splitter.CleanUp(pieces)

	err = app.recordParts(id, pieces)
	if err != nil {
		return nil, err
	}
	return pieces, nil
}

func (app Syncer) putObjs(ctx context.Context, objs []string, deep bool) error {
	spinnerInfo, err := pterm.DefaultSpinner.Start("uploading parts")
	if err != nil {
		return err
	}

	for i, obj := range objs {
		spinnerInfo.UpdateText(fmt.Sprintf("Uploading %s part %d/%d", obj, i+1, len(objs)))
		err = app.putObject(ctx, obj, spinnerInfo, deep)
		if err != nil {
			return err
		}
		// update the upload status on the parts
		err = app.updateUploadStatusPart(obj)
		if err != nil {
			return err
		}
	}
	spinnerInfo.Success(fmt.Sprintf("Uploaded parts 0 - %d", len(objs)))
	return nil
}

// get lastModDate returns the last moidified date for the file specified by f (file path).
// Returns unix time
func getLastModDate(f string) (int64, error) {
	fileinfo, err := os.Stat(f)
	if err != nil {
		return 0, err
	}
	atime := fileinfo.ModTime().Unix()
	return atime, nil
}
