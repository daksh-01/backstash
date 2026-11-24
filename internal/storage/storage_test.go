package storage

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is just enough of the S3 API (path-style) to exercise the real SDK client.
type fakeS3 struct {
	mu         sync.Mutex
	objects    map[string][]byte
	mtimes     map[string]time.Time
	putHeaders []http.Header
}

func newFakeS3(testState *testing.T) (*fakeS3, *S3) {
	testState.Helper()
	fake := &fakeS3{objects: map[string][]byte{}, mtimes: map[string]time.Time{}}
	srv := httptest.NewServer(fake)
	testState.Cleanup(srv.Close)
	return fake, NewS3(S3Config{Endpoint: srv.URL, Region: "auto", Bucket: "bkt", AccessKeyID: "id", SecretAccessKey: "secret"})
}

func (fake *fakeS3) ServeHTTP(respWriter http.ResponseWriter, req *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	key := strings.TrimPrefix(req.URL.Path, "/bkt/")
	switch {
	case req.Method == http.MethodPut:
		body, _ := io.ReadAll(req.Body)
		fake.putHeaders = append(fake.putHeaders, req.Header.Clone())
		fake.objects[key] = body
		fake.mtimes[key] = time.Now()
		respWriter.Header().Set("ETag", `"x"`)
	case req.Method == http.MethodGet && req.URL.Query().Get("list-type") == "2":
		prefix := req.URL.Query().Get("prefix")
		var keys []string
		for objKey := range fake.objects {
			if strings.HasPrefix(objKey, prefix) {
				keys = append(keys, objKey)
			}
		}
		sort.Strings(keys)
		type obj struct {
			Key          string
			Size         int
			LastModified string
		}
		out := struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []obj
		}{}
		for _, objKey := range keys {
			out.Contents = append(out.Contents, obj{objKey, len(fake.objects[objKey]), fake.mtimes[objKey].UTC().Format(time.RFC3339)})
		}
		_ = xml.NewEncoder(respWriter).Encode(out)
	case req.Method == http.MethodGet:
		body, ok := fake.objects[key]
		if !ok {
			respWriter.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(respWriter, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		_, _ = respWriter.Write(body)
	case req.Method == http.MethodPost && req.URL.Query().Has("delete"):
		var in struct {
			Objects []struct{ Key string } `xml:"Object"`
		}
		_ = xml.NewDecoder(req.Body).Decode(&in)
		for _, objItem := range in.Objects {
			delete(fake.objects, objItem.Key)
		}
		_, _ = io.WriteString(respWriter, `<DeleteResult/>`)
	default:
		respWriter.WriteHeader(http.StatusNotImplemented)
	}
}

func TestUploadListDownloadDelete(testState *testing.T) {
	fake, st := newFakeS3(testState)
	ctx := context.Background()
	src := filepath.Join(testState.TempDir(), "a.dump")
	_ = os.WriteFile(src, []byte("PGDMP-data"), 0o600)

	if err := st.Upload(ctx, src, "myapp/prod/postgres/a-20261123-020005.dump"); err != nil {
		testState.Fatal(err)
	}
	if err := st.Upload(ctx, src, "other/b.dump"); err != nil {
		testState.Fatal(err)
	}

	objs, err := st.List(ctx, "myapp/prod/postgres/")
	if err != nil || len(objs) != 1 || objs[0].Size != 10 || objs[0].Key != "myapp/prod/postgres/a-20261123-020005.dump" {
		testState.Fatalf("list = %+v, %v", objs, err)
	}

	dest := filepath.Join(testState.TempDir(), "got.dump")
	if err := st.Download(ctx, objs[0].Key, dest); err != nil {
		testState.Fatal(err)
	}
	if body, _ := os.ReadFile(dest); string(body) != "PGDMP-data" {
		testState.Errorf("downloaded %q", body)
	}
	if info, _ := os.Stat(dest); info.Mode().Perm() != 0o600 {
		testState.Errorf("download mode = %v", info.Mode().Perm())
	}

	if err := st.Delete(ctx, []string{objs[0].Key}); err != nil {
		testState.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, still := fake.objects[objs[0].Key]; still || len(fake.objects) != 1 {
		testState.Errorf("objects after delete: %v", fake.objects)
	}
}

func TestDownloadMissingKeyFails(testState *testing.T) {
	_, st := newFakeS3(testState)
	dest := filepath.Join(testState.TempDir(), "x")
	if err := st.Download(context.Background(), "nope", dest); err == nil {
		testState.Fatal("expected an error")
	}
	if _, err := os.Stat(dest); err == nil {
		testState.Error("no file should be created for a failed download")
	}
}

// R2 and MinIO reject the checksum trailers newer SDKs add by default. The transfer manager has its
// own setting that overrides the client's, so this guards against a silent regression there.
func TestUploadSendsNoChecksumHeaders(testState *testing.T) {
	fake, st := newFakeS3(testState)
	src := filepath.Join(testState.TempDir(), "a.dump")
	_ = os.WriteFile(src, []byte("PGDMP-data"), 0o600)
	if err := st.Upload(context.Background(), src, "a/b.dump"); err != nil {
		testState.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.putHeaders) != 1 {
		testState.Fatalf("want 1 PUT, got %d", len(fake.putHeaders))
	}
	for name, vals := range fake.putHeaders[0] {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-checksum") || lower == "x-amz-sdk-checksum-algorithm" || lower == "x-amz-trailer" {
			testState.Errorf("upload sent checksum header %s=%v", name, vals)
		}
	}
	if enc := fake.putHeaders[0].Get("Content-Encoding"); strings.Contains(enc, "aws-chunked") {
		testState.Errorf("upload used aws-chunked encoding: %q", enc)
	}
}
