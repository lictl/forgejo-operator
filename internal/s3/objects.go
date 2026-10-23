package s3

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Object is an entry of a bucket listing.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// listPageSize is the number of keys asked for per ListObjectsV2 request (the S3 maximum).
const listPageSize = 1000

// GetObject reads the object key. It fails when the object is larger than limit bytes.
func GetObject(ctx context.Context, b Bucket, key string, limit int64) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("empty object key")
	}
	return b.send(ctx, http.MethodGet, key, nil, limit)
}

// DeleteObject removes the object key. Deleting a key that does not exist succeeds, as in S3.
func DeleteObject(ctx context.Context, b Bucket, key string) error {
	if key == "" {
		return fmt.Errorf("empty object key")
	}
	_, err := b.send(ctx, http.MethodDelete, key, nil, 64<<10)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ListObjects returns the objects whose keys start with prefix (ListObjectsV2), in key order,
// at most limit of them; more keys than that is an error.
func ListObjects(ctx context.Context, b Bucket, prefix string, limit int) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "max-keys": {strconv.Itoa(listPageSize)}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		body, err := b.send(ctx, http.MethodGet, "", q, 16<<20)
		if err != nil {
			return nil, err
		}
		var page struct {
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
			Contents              []struct {
				Key          string `xml:"Key"`
				Size         int64  `xml:"Size"`
				LastModified string `xml:"LastModified"`
			} `xml:"Contents"`
		}
		if err := xml.Unmarshal(body, &page); err != nil {
			return nil, &Error{Reason: ReasonFailed, Message: fmt.Sprintf("listing %q: parsing the answer: %v", prefix, err)}
		}
		for _, c := range page.Contents {
			o := Object{Key: c.Key, Size: c.Size}
			if t, err := time.Parse(time.RFC3339Nano, c.LastModified); err == nil {
				o.LastModified = t
			}
			out = append(out, o)
		}
		if len(out) > limit {
			return nil, &Error{Reason: ReasonFailed, Message: fmt.Sprintf("listing %q: more than %d objects", prefix, limit)}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}
