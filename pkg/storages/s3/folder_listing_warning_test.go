package s3_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/require"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/pkg/storages/s3"
	"github.com/wal-g/wal-g/pkg/storages/storage"
)

type listingWarningClient struct {
	mockS3ClientVersioning
	calls     int
	failPage  int
	errorCode string
	emptyPage bool
}

func (c *listingWarningClient) nextPage() error {
	c.calls++
	if c.calls == c.failPage {
		return awserr.New(c.errorCode, "listing failed", nil)
	}
	return nil
}

func (c *listingWarningClient) ListObjectsV2Pages(_ *awss3.ListObjectsV2Input,
	callback func(*awss3.ListObjectsV2Output, bool) bool) error {
	for c.calls < 2 {
		if err := c.nextPage(); err != nil {
			return err
		}
		out := &awss3.ListObjectsV2Output{}
		if !c.emptyPage {
			out.Contents = []*awss3.Object{{
				Key:          aws.String(fmt.Sprintf("archive/wal_005/0000000800000002%08d.lz4", c.calls)),
				LastModified: aws.Time(time.Unix(0, 0)), Size: aws.Int64(123),
			}}
		}
		if !callback(out, c.calls == 2) {
			break
		}
	}
	return nil
}

func (c *listingWarningClient) ListObjectsPages(_ *awss3.ListObjectsInput,
	callback func(*awss3.ListObjectsOutput, bool) bool) error {
	return c.ListObjectsV2Pages(nil, func(out *awss3.ListObjectsV2Output, last bool) bool {
		return callback(&awss3.ListObjectsOutput{Contents: out.Contents}, last)
	})
}

func (c *listingWarningClient) ListObjectsV2PagesWithContext(_ aws.Context, input *awss3.ListObjectsV2Input,
	callback func(*awss3.ListObjectsV2Output, bool) bool, _ ...request.Option) error {
	return c.ListObjectsV2Pages(input, callback)
}

func (c *listingWarningClient) ListObjectsPagesWithContext(_ aws.Context, input *awss3.ListObjectsInput,
	callback func(*awss3.ListObjectsOutput, bool) bool, _ ...request.Option) error {
	return c.ListObjectsPages(input, callback)
}

func (c *listingWarningClient) ListObjectVersionsPages(_ *awss3.ListObjectVersionsInput,
	callback func(*awss3.ListObjectVersionsOutput, bool) bool) error {
	for c.calls < 2 {
		if err := c.nextPage(); err != nil {
			return err
		}
		out := &awss3.ListObjectVersionsOutput{}
		if !c.emptyPage {
			out.Versions = []*awss3.ObjectVersion{{
				Key:          aws.String(fmt.Sprintf("archive/wal_005/0000000800000002%08d.lz4", c.calls)),
				LastModified: aws.Time(time.Unix(0, 0)), Size: aws.Int64(123),
				VersionId: aws.String("v1"), IsLatest: aws.Bool(true),
			}}
		}
		if !callback(out, c.calls == 2) {
			break
		}
	}
	return nil
}

func TestListingMissingPageWarning(t *testing.T) {
	for _, mode := range []string{"plain", "plain v1", "prefix", "prefix v1", "versions"} {
		for _, tc := range []struct {
			name        string
			code        string
			failPage    int
			emptyPage   bool
			wantObjects int
			wantWarning bool
			wantError   bool
		}{
			{name: "success", wantObjects: 2},
			{name: "first missing key", code: "NoSuchKey", failPage: 1},
			{name: "first not found", code: "NotFound", failPage: 1},
			{name: "later missing key", code: "NoSuchKey", failPage: 2, wantObjects: 1, wantWarning: true},
			{name: "later not found", code: "NotFound", failPage: 2, wantObjects: 1, wantWarning: true},
			{name: "empty page then missing key", code: "NoSuchKey", failPage: 2, emptyPage: true, wantWarning: true},
			{name: "empty page then not found", code: "NotFound", failPage: 2, emptyPage: true, wantWarning: true},
			{name: "first denied", code: "AccessDenied", failPage: 1, wantError: true},
			{name: "later denied", code: "AccessDenied", failPage: 2, wantError: true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var warnings bytes.Buffer
				defer tracelog.WarningLogger.SetOutput(tracelog.WarningLogger.Writer())
				tracelog.WarningLogger.SetOutput(&warnings)
				client := &listingWarningClient{failPage: tc.failPage, errorCode: tc.code, emptyPage: tc.emptyPage}
				config := &s3.Config{Bucket: "bucket", EnableVersioning: "disabled", UseListObjectsV1: strings.HasSuffix(mode, "v1")}
				if mode == "versions" {
					config.EnableVersioning = "enabled"
				}
				folder := s3.NewFolder(client, nil, "archive/wal_005/", config)
				var objects []storage.Object
				var err error
				if strings.HasPrefix(mode, "prefix") {
					objects, err = folder.ListObjectsWithPrefix(context.Background(), "0000000800000002")
				} else {
					objects, _, err = folder.ListFolder()
				}
				if tc.wantError {
					require.ErrorContains(t, err, "AccessDenied")
				} else {
					require.NoError(t, err)
				}
				require.Len(t, objects, tc.wantObjects)
				for i, object := range objects {
					require.Equal(t, fmt.Sprintf("0000000800000002%08d.lz4", i+1), object.GetName())
				}
				if tc.wantWarning {
					require.Equal(t, 1, strings.Count(warnings.String(), "WARNING:"))
					require.Contains(t, warnings.String(), "S3 listing incomplete after successful pages")
					require.Contains(t, warnings.String(), "completed_pages=1")
					require.Contains(t, warnings.String(), tc.code)
				} else {
					require.Empty(t, warnings.String())
				}
				wantCalls := 2
				if tc.failPage == 1 {
					wantCalls = 1
				}
				require.Equal(t, wantCalls, client.calls)
			})
		}
	}
}
