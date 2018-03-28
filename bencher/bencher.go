// Copyright 2018, OpenCensus Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bencher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/build"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"golang.org/x/perf/benchstat"

	"go.opencensus.io/trace"

	"github.com/keighl/postmark"
	"github.com/orijtech/infra"
)

const unchanged = int(0)

func runGoBenchmarks(ctx context.Context, gitRepoURL string) ([]byte, error) {
	ctx, span := trace.StartSpan(ctx, "/run-go-benchmarks")
	defer span.End()

	// 1. Change directories to the target Go project
	cmd := exec.CommandContext(ctx, "go", "test", "-run=^$", "-bench=.", "-count=5", "./...")
	cmd.Dir = filepath.Join(build.Default.GOPATH, "src", gitRepoURL)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	// Filter out anything that doesn't begin with a benchmark
	lines := strings.Split(string(output), "\n")
	var benchmarkLines []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Benchmark") {
			benchmarkLines = append(benchmarkLines, line)
		}
	}
	if len(benchmarkLines) == 0 {
		return nil, ErrNoBenchmarks
	}
	return []byte(strings.Join(benchmarkLines, "\n")), nil
}

type Request struct {
	AppEmail         string        `json:"app_email"`
	AppSecret        string        `json:"app_secret"`
	GCSBucket        string        `json:"gcs_bucket"`
	GCSProject       string        `json:"gcs_project"`
	GitRepoURL       string        `json:"git_repo_url"`
	AlertEmails      []string      `json:"alert_emails"`
	Secret           string        `json:"secret"`
	Public           bool          `json:"public"`
	EmailServerToken string        `json:"email_server_token"`
	EmailAccountToken string        `json:"email_client_token"`
	InfraClient      *infra.Client `json:"infra_client"`
}

func (br *Request) BenchmarkAndEmail(ctx context.Context) (interface{}, error) {
	ctx, span := trace.StartSpan(ctx, "/benchmark-and-email")
	defer span.End()

	// 1. TODO: Match up those secrets and validate!

	// 2. Run those benchmarks
	results, err := br.Benchmark(ctx)
	if err != nil {
		return nil, err
	}

	toEmails := strings.Join(br.AlertEmails, ",")
	htmlBuf := new(bytes.Buffer)
	if err := emailTmpl.Execute(htmlBuf, results); err != nil {
		return nil, err
	}

	pmClient := postmark.NewClient(br.EmailServerToken, br.EmailAccountToken)
	email := postmark.Email{
		From:     br.AppEmail,
		To:       toEmails,
		Subject:  fmt.Sprintf("Benchmarks for %s", br.GitRepoURL),
		HtmlBody: htmlBuf.String(),
	}

	if _, err := pmClient.SendEmail(email); err != nil {
		return results, err
	}

	return results, nil
}

var (
	ErrNoChanges    = errors.New("no changes detected!")
	ErrNoBenchmarks = errors.New("no benchmarks found!")
)

type Result struct {
	URLs           map[string]string
	Benchmarks     string
	HTMLBenchmarks string
}

var pmClient = postmark.NewClient(os.Getenv("BENCHER_POSTMARK_SERVER_TOKEN"), os.Getenv("BENCHER_POSTMARK_CLIENT_TOKEN"))

func (br *Request) Benchmark(ctx context.Context) (interface{}, error) {
	ctx, span := trace.StartSpan(ctx, "/benchmark")
	defer span.End()

	// 1. Check out the branch if necessary
	// 2. Run the tests
	// 3. Get the before and after

	afterBlob, err := runGoBenchmarks(ctx, br.GitRepoURL)
	if err != nil {
		return nil, err
	}
	return br.uploadToGCS(ctx, afterBlob)
}

func (br *Request) uploadToGCS(ctx context.Context, afterBlob []byte) (interface{}, error) {
	ctx, span := trace.StartSpan(ctx, "/upload-to-gcs")
	defer span.End()

	inBenchmarksDir := func(suffix string) string {
		return br.GitRepoURL + "/benchmarks/" + suffix
	}

	now := time.Now()
	nowUniqPrefix := fmt.Sprintf("%d-%d-%d/%d", now.Year(), now.Month(), now.Day(), now.Unix())

	infraClient := br.InfraClient

	// 1. Check if the cloud listing exists
	obj, err := infraClient.Object(br.GCSBucket, inBenchmarksDir("latest"))
	if err != nil || obj == nil {
		ctx, span := trace.StartSpan(ctx, "/non-existent-benchmarks")
		defer span.End()

		results := map[string]string{}
		// log.Printf("Most likely the stored benchmarks don't yet exist!")

		paths := []string{"latest", nowUniqPrefix}
		for _, path := range paths {
			url, err := uploadBenchmarksToGCS(ctx, &definition{
				GCSProject: br.GCSProject,
				Bucket:     br.GCSBucket,
				Name:       inBenchmarksDir(path),
				Public:     br.Public,
				Reader: func() io.Reader {
					return bytes.NewReader(afterBlob)
				},
				infraClient: infraClient,
			})
			if err != nil {
				return results, fmt.Errorf("Uploading benchmarks first-time: %v", err)
			}
			results[path] = url
		}
		return &Result{URLs: results, Benchmarks: string(afterBlob)}, nil
	}

	ctx, dlSpan := trace.StartSpan(ctx, "/download-existent-benchmarks")
	// 2. Otherwise, retrieve those benchmarks since they exist.
	brc, err := infraClient.Download(br.GCSBucket, inBenchmarksDir("latest"))
	dlSpan.End()

	if err != nil {
		return nil, fmt.Errorf("Retrieving `before` benchmarks: %v", err)
	}
	beforeBuffer := new(bytes.Buffer)
	_, err = io.Copy(beforeBuffer, brc)
	_ = brc.Close()
	if err != nil {
		return nil, fmt.Errorf("Downloading `before` benchmarks: %v", err)
	}

	c := &benchstat.Collection{
		Alpha:      0.05,
		AddGeoMean: false,
		DeltaTest:  benchstat.UTest,
		SplitBy:    []string{"pkg", "goos", "goarch"},
	}
	c.AddConfig("before", beforeBuffer.Bytes())
	c.AddConfig("after", afterBlob)

	ctx, computeTablesSpan := trace.StartSpan(ctx, "/compute-benchmark-differences")
	// 3. Now generate those benchmarks
	tables := c.Tables()
	// Filter out the unchanged values
	var changed []*benchstat.Table
	for _, table := range tables {
		var rows []*benchstat.Row
		for _, row := range table.Rows {
			if row.Change != unchanged {
				rows = append(rows, row)
			}
		}
		if len(rows) == 0 {
			continue
		}

		table.Rows = rows
		// Otherwise now swap out the old rows
		// and this is a changed table result.
		changed = append(changed, table)
	}
	computeTablesSpan.End()

	if len(changed) == 0 {
		return nil, ErrNoChanges
	}

	// 4. Now update/replace the already existent benchmarks
	newBenchmarksReaderFunc := func() io.Reader {
		buf := new(bytes.Buffer)
		benchstat.FormatText(buf, changed)
		return buf
	}

	uploads := []struct {
		rfn   func() io.Reader
		paths []string
	}{
		{
			paths: []string{
				"latest",
				nowUniqPrefix,
			},
			rfn: func() io.Reader { return bytes.NewReader(afterBlob) },
		},
		{
			paths: []string{
				"latest-results",
				nowUniqPrefix + "-results",
			},
			rfn: newBenchmarksReaderFunc,
		},
	}

	ctx, uploadsSpan := trace.StartSpan(ctx, "/perform-uploads")
	defer uploadsSpan.End()

	urls := make(map[string]string)
	for _, upload := range uploads {
		for _, path := range upload.paths {
			def := &definition{
				GCSProject:  br.GCSProject,
				Bucket:      br.GCSBucket,
				Name:        inBenchmarksDir(path),
				Public:      br.Public,
				Reader:      upload.rfn,
				infraClient: infraClient,
			}
			url, err := uploadBenchmarksToGCS(ctx, def)
			if err != nil {
				return nil, fmt.Errorf("uploadBenchmarksToGCS: %q: %v", path, err)
			}
			urls[path] = url
		}
	}

	htmlBuf := new(bytes.Buffer)
	benchstat.FormatHTML(htmlBuf, changed)
	res := &Result{
		URLs:           urls,
		Benchmarks:     newBenchmarksReaderFunc().(*bytes.Buffer).String(),
		HTMLBenchmarks: htmlBuf.String(),
	}
	return res, nil
}

type definition struct {
	Name        string
	GCSProject  string
	Bucket      string
	Reader      func() io.Reader
	Public      bool
	infraClient *infra.Client
}

func uploadBenchmarksToGCS(ctx context.Context, def *definition) (string, error) {
	ctx, span := trace.StartSpan(ctx, "/upload-benchmarks-to-gcs")
	defer span.End()

	ic := def.infraClient
	// 1. Ensure that the bucket exists on GCS
	bc := &infra.BucketCheck{Project: def.GCSProject, Bucket: def.Bucket}
	if _, err := ic.EnsureBucketExists(bc); err != nil {
		return "", err
	}

	// 2. Upload the benchmarks
	params := &infra.UploadParams{
		Bucket: def.Bucket,
		Name:   def.Name,
		Reader: def.Reader,
		Public: def.Public,
	}
	obj, err := ic.UploadWithParams(params)
	if err != nil {
		return "", err
	}
	return infra.ObjectURL(obj), nil
}

var emailTmpl = template.Must(template.New("email").Parse(`
{{if .HTMLBenchmarks}}
{{.HTMLBenchmarks}}

{{end}}

<br />
{{if .URLs}}
  The respective URLs are:
<br />
{{range $key, $value := .URLs}}
{{$key}} : {{$value}}
<br />

{{end}}
{{end}}
`))
