package s3remote

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/common"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

// FS represents filesystem for backups in S3.
//
// Init must be called before calling other FS methods.
type FS struct {
	// Path to S3 credentials file.
	CredsFilePath string

	// Pat to S3 configs file.
	ConfigFilePath string

	// GCS bucket to use.
	Bucket string

	// Directory in the bucket to write to.
	Dir string

	s3       *s3.S3
	uploader *s3manager.Uploader
}

// Init initializes fs.
func (fs *FS) Init() error {
	if fs.s3 != nil {
		logger.Panicf("BUG: Init is already called")
	}
	for strings.HasPrefix(fs.Dir, "/") {
		fs.Dir = fs.Dir[1:]
	}
	if !strings.HasSuffix(fs.Dir, "/") {
		fs.Dir += "/"
	}
	opts := session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}
	if len(fs.CredsFilePath) > 0 {
		opts.SharedConfigFiles = []string{
			fs.ConfigFilePath,
			fs.CredsFilePath,
		}
	}
	sess, err := session.NewSessionWithOptions(opts)
	if err != nil {
		return fmt.Errorf("cannot create S3 session: %s", err)
	}

	// Determine bucket region.
	ctx := context.Background()
	region, err := s3manager.GetBucketRegion(ctx, sess, fs.Bucket, "")
	if err != nil {
		return fmt.Errorf("cannot determine region for bucket %q: %s", fs.Bucket, err)
	}
	sess.Config.WithRegion(region)
	logger.Infof("bucket %q is stored at region %q; switching to this region", fs.Bucket, region)

	fs.s3 = s3.New(sess)
	fs.uploader = s3manager.NewUploader(sess, func(u *s3manager.Uploader) {
		// We manage upload concurrency by ourselves.
		u.Concurrency = 1
	})
	return nil
}

// String returns human-readable description for fs.
func (fs *FS) String() string {
	return fmt.Sprintf("S3{bucket: %q, dir: %q}", fs.Bucket, fs.Dir)
}

// ListParts returns all the parts for fs.
func (fs *FS) ListParts() ([]common.Part, error) {
	dir := fs.Dir
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(fs.Bucket),
		Prefix: aws.String(dir),
	}
	var errOuter error
	var parts []common.Part
	err := fs.s3.ListObjectsV2Pages(input, func(page *s3.ListObjectsV2Output, lastPage bool) bool {
		for _, o := range page.Contents {
			file := *o.Key
			if !strings.HasPrefix(file, dir) {
				errOuter = fmt.Errorf("unexpected prefix for s3 key %q; want %q", file, dir)
				return false
			}
			var p common.Part
			if !p.ParseFromRemotePath(file[len(dir):]) {
				logger.Infof("skipping unknown object %q", file)
				continue
			}
			p.ActualSize = uint64(*o.Size)
			parts = append(parts, p)
		}
		return !lastPage
	})
	if errOuter != nil && err == nil {
		err = errOuter
	}
	if err != nil {
		return nil, fmt.Errorf("error when listing s3 objects inside dir %q: %s", dir, err)
	}
	return parts, nil
}

// DeletePart deletes part p from fs.
func (fs *FS) DeletePart(p common.Part) error {
	path := fs.path(p)
	input := &s3.DeleteObjectInput{
		Bucket: aws.String(fs.Bucket),
		Key:    aws.String(path),
	}
	_, err := fs.s3.DeleteObject(input)
	if err != nil {
		return fmt.Errorf("cannot delete %q at %s (remote path %q): %s", p.Path, fs, path, err)
	}
	return nil
}

// RemoveEmptyDirs recursively removes empty dirs in fs.
func (fs *FS) RemoveEmptyDirs() error {
	// S3 has no directories, so nothing to remove.
	return nil
}

// CopyPart copies p from srcFS to fs.
func (fs *FS) CopyPart(srcFS common.OriginFS, p common.Part) error {
	src, ok := srcFS.(*FS)
	if !ok {
		return fmt.Errorf("cannot perform server-side copying from %s to %s: both of them must be S3", srcFS, fs)
	}
	srcPath := src.path(p)
	dstPath := fs.path(p)
	copySource := fmt.Sprintf("/%s/%s", src.Bucket, srcPath)

	input := &s3.CopyObjectInput{
		Bucket:     aws.String(fs.Bucket),
		CopySource: aws.String(copySource),
		Key:        aws.String(dstPath),
	}
	_, err := fs.s3.CopyObject(input)
	if err != nil {
		return fmt.Errorf("cannot copy %q from %s to %s (copySource %q): %s", p.Path, src, fs, copySource, err)
	}
	return nil
}

// DownloadPart downloads part p from fs to w.
func (fs *FS) DownloadPart(p common.Part, w io.Writer) error {
	path := fs.path(p)
	input := &s3.GetObjectInput{
		Bucket: aws.String(fs.Bucket),
		Key:    aws.String(path),
	}
	o, err := fs.s3.GetObject(input)
	if err != nil {
		return fmt.Errorf("cannot open %q at %s (remote path %q): %s", p.Path, fs, path, err)
	}
	r := o.Body
	n, err := io.Copy(w, r)
	if err1 := r.Close(); err1 != nil && err == nil {
		err = err1
	}
	if err != nil {
		return fmt.Errorf("cannot download %q from at %s (remote path %q): %s", p.Path, fs, path, err)
	}
	if uint64(n) != p.Size {
		return fmt.Errorf("wrong data size downloaded from %q at %s; got %d bytes; want %d bytes", p.Path, fs, n, p.Size)
	}
	return nil
}

// UploadPart uploads part p from r to fs.
func (fs *FS) UploadPart(p common.Part, r io.Reader) error {
	path := fs.path(p)
	sr := &statReader{
		r: r,
	}
	input := &s3manager.UploadInput{
		Bucket: aws.String(fs.Bucket),
		Key:    aws.String(path),
		Body:   sr,
	}
	_, err := fs.uploader.Upload(input)
	if err != nil {
		return fmt.Errorf("cannot upoad data to %q at %s (remote path %q): %s", p.Path, fs, path, err)
	}
	if uint64(sr.size) != p.Size {
		return fmt.Errorf("wrong data size uploaded to %q at %s; got %d bytes; want %d bytes", p.Path, fs, sr.size, p.Size)
	}
	return nil
}

func (fs *FS) path(p common.Part) string {
	return p.RemotePath(fs.Dir)
}

type statReader struct {
	r    io.Reader
	size int64
}

func (sr *statReader) Read(p []byte) (int, error) {
	n, err := sr.r.Read(p)
	sr.size += int64(n)
	return n, err
}
