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

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/perf/benchstat"

	"github.com/keighl/postmark"
	"github.com/orijtech/infra"
)

const unchanged = int(0)

func runGoBenchmarks(ctx context.Context, gitRepoURL string) ([]byte, error) {
	// 1. Change directories to the target Go project
	log.Printf("Starting benchmarks for %q, this may take a while!", gitRepoURL)
	defer log.Printf("Done running benchmarks!")
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
		return nil, errNoBenchmarks
	}
	return []byte(strings.Join(benchmarkLines, "\n")), nil
}

type benchmarkRequest struct {
	GitRepoURL  string   `json:"git_repo_url"`
	AlertEmails []string `json:"alert_emails"`
	Secret      string   `json:"secret"`
	Public      bool     `json:"public"`
}

var gcsBucket, gcsProject string

func main() {
	log.SetFlags(0)

	var port int
	flag.IntVar(&port, "port", 7788, "the port to run the server")
	flag.StringVar(&gcsBucket, "bucket", "census-demos", "the GCS bucket to use")
	flag.StringVar(&gcsProject, "project", "census-demos", "the GCS project to use")
	flag.Parse()

	mux := http.NewServeMux()
	mux.Handle("/benchmark", http.HandlerFunc(handleBenchmarkPing))
	mux.Handle("/health", http.HandlerFunc(health))

	addr := fmt.Sprintf(":%d", port)
	log.Printf("Running bencher server at %q", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}

func handleBenchmarkPing(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	br := new(benchmarkRequest)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&br); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// 1. TODO: Match up those secrets

	// 2. Run those benchmarks
	results, err := benchmarkIt(r.Context(), br)

	if err == errNoChanges {
		fmt.Fprintf(w, "No changes detected!")
		return
	}

	if err != nil {
		// A generic error
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	toEmails := strings.Join(br.AlertEmails, ",")
	htmlBuf := new(bytes.Buffer)
	if err := emailTmpl.Execute(htmlBuf, results); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	email := postmark.Email{
		From:     "emmanuel@orijtech.com",
		To:       toEmails,
		Subject:  fmt.Sprintf("RE: Benchmarks for %s", br.GitRepoURL),
		HtmlBody: htmlBuf.String(),
	}

	if _, err := pmClient.SendEmail(email); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 3. TODO: Now compose the email
	blob, _ := json.Marshal(results)
	_, _ = w.Write(blob)
}

func health(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Alive\n\n%d\n", time.Now().Unix())
}

var (
	errNoChanges    = errors.New("no changes detected!")
	errNoBenchmarks = errors.New("no benchmarks found!")
)

type outputData struct {
	Urls           map[string]string
	Benchmarks     string
	HTMLBenchmarks string
}

var pmClient = postmark.NewClient(os.Getenv("BENCHER_POSTMARK_SERVER_TOKEN"), os.Getenv("BENCHER_POSTMARK_CLIENT_TOKEN"))

func benchmarkIt(ctx context.Context, br *benchmarkRequest) (interface{}, error) {
	infraClient, err := infra.NewDefaultClient()
	if err != nil {
		return nil, err
	}

	// 1. Check out the branch if necessary
	// 2. Run the tests
	// 3. Get the before and after

	afterBlob, err := runGoBenchmarks(ctx, br.GitRepoURL)
	if err != nil {
		return nil, err
	}

	inBenchmarksDir := func(suffix string) string {
		return br.GitRepoURL + "/benchmarks/" + suffix
	}

	now := time.Now()
	nowUniqPrefix := fmt.Sprintf("%d-%d-%d/%d", now.Year(), now.Month(), now.Day(), now.Unix())

	// 1. Check if the cloud listing exists
	obj, err := infraClient.Object(gcsBucket, inBenchmarksDir("latest"))
	if err != nil || obj == nil {
		results := map[string]string{}
		log.Printf("Most likely the stored benchmarks don't yet exist!")

		paths := []string{"latest", nowUniqPrefix}
		for _, path := range paths {
			url, err := uploadBenchmarksToGCS(&definition{
				GCSProject: gcsProject,
				Bucket:     gcsBucket,
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
		return &outputData{Urls: results, Benchmarks: string(afterBlob)}, nil
	}

	// 2. Otherwise, retrieve those benchmarks since they exist.
	brc, err := infraClient.Download(gcsBucket, inBenchmarksDir("latest"))
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

	if len(changed) == 0 {
		return nil, errNoChanges
	}

	// 4. Now update/replace the already existent benchmarks
	newBenchmarksReaderFunc := func() io.Reader {
		buf := new(bytes.Buffer)
		benchstat.FormatText(buf, changed)
		return buf
	}

	// log.Printf("New changes detected:\n%s\n\n", newBenchmarksReaderFunc().(*bytes.Buffer).Bytes())

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

	results := make(map[string]string)
	for _, upload := range uploads {
		for _, path := range upload.paths {
			def := &definition{
				GCSProject:  gcsProject,
				Bucket:      gcsBucket,
				Name:        inBenchmarksDir(path),
				Public:      br.Public,
				Reader:      upload.rfn,
				infraClient: infraClient,
			}
			url, err := uploadBenchmarksToGCS(def)
			if err != nil {
				return nil, fmt.Errorf("uploadBenchmarksToGCS: %q: %v", path, err)
			}
			results[path] = url
		}
	}

	htmlBuf := new(bytes.Buffer)
	benchstat.FormatHTML(htmlBuf, changed)
	res := &outputData{
		Urls:           results,
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

func uploadBenchmarksToGCS(def *definition) (string, error) {
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
{{if .Urls}}
  The respective URLs are:
<br />
{{range $key, $value := .Urls}}
{{$key}} : {{$value}}
<br />

{{end}}
{{end}}
`))
