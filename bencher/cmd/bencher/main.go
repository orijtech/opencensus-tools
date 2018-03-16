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
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/orijtech/infra"
	"github.com/orijtech/opencensus-tools/bencher"
)

var (
	gcsBucket, appEmail, gcsProject string

	postmarkServerToken = os.Getenv("BENCHER_POSTMARK_SERVER_TOKEN")
	postmarkAccountToken = os.Getenv("BENCHER_POSTMARK_ACCOUNT_TOKEN")

	infraClient *infra.Client
)

func main() {
	log.SetFlags(0)

	var port int
	var http2 bool
	var domains string
	flag.IntVar(&port, "port", 7788, "the port to run the server")
	flag.StringVar(&gcsBucket, "bucket", "census-demos", "the GCS bucket to use")
	flag.StringVar(&gcsProject, "project", "census-demos", "the GCS project to use")
	flag.StringVar(&appEmail, "app-email", "emmanuel@orijtech.com", "the email for the app")
	flag.BoolVar(&http2, "http2", false, "whether to run it as an HTTP/2 and HTTPS enabled server")
	flag.StringVar(&domains, "domains", "", "the comma separated list of domains e.g. foo.example.org,baz.example.com")
	flag.Parse()

	mux := http.NewServeMux()
	mux.Handle("/benchmark", http.HandlerFunc(handleBenchmarking))
	mux.Handle("/ping", http.HandlerFunc(health))

	// Set the infra client
	var err error
	infraClient, err = infra.NewDefaultClient()
	if err != nil {
		log.Fatalf("NewDefaultClient: %v", err)
	}

	if !http2 {
		addr := fmt.Sprintf(":%d", port)
		log.Printf("Running non-HTTP/2 bencher server at %q", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatalf("ListenAndServe: %v", err)
		}
		return
	}

	allDomains := strings.Split(domains, ",")
	if len(allDomains) == 0 || strings.TrimSpace(allDomains[0]) == "" {
		log.Fatal("expecting at least one non-blank domain, separated by comma if many")
	}
	// Otherwise time to run it as an HTTP/2 and HTTPS enabled server
	log.Fatal(http.Serve(autocert.NewListener(allDomains...), mux))
}

type benchRequest struct {
	AppSecret   string   `json:"app_secret"`
	GitRepoURL  string   `json:"git_repo_url"`
	AlertEmails []string `json:"alert_emails"`
	Secret      string   `json:"secret"`
	Public      bool     `json:"public"`
}

func handleBenchmarking(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	br := new(benchRequest)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&br); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// 1. TODO: Match up those secrets

	brq := &bencher.Request{
		AppEmail:         appEmail,
		EmailServerToken: postmarkServerToken,
		AlertEmails:      br.AlertEmails,
		EmailAccountToken: postmarkAccountToken,
		InfraClient:      infraClient,
		GitRepoURL:       br.GitRepoURL,
		GCSBucket:        gcsBucket,
		GCSProject:       gcsProject,
		Public:           br.Public,
		Secret:           br.Secret,
	}

	// 2. Run those benchmarks
	results, err := brq.BenchmarkAndEmail(r.Context())

	switch {
	case err == bencher.ErrNoChanges:
		fmt.Fprintf(w, "No changes detected!")
		return

	case err != nil:
		// A generic error
		http.Error(w, err.Error(), http.StatusBadRequest)
		return

	default:
		blob, _ := json.Marshal(results)
		_, _ = w.Write(blob)
	}
}

func health(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Alive\n\n%d\n", time.Now().Unix())
}
