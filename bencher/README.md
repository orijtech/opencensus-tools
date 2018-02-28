## bencher

Go benchmarking utility that's accessible via the web. It runs your benchmarks
for the repo and uploads results to Google Cloud Storage(GCS). For every fresh run
of benchmarks in a repository, it checks if GCS had already run benchmarks
and compares with those. If there are any changes, it will email the respective
parties.

### Running it

#### Server
* Server prerequisites

Name|Validation|Default|Info
---|---|---|---
bucket|a non blank string|census-demos|The GCS bucket in which your benchmarking results will be saved
port|an integer in the range [0, 65536]|7788|The port on which we should run the server
project|a non blank string|census-demos|The GCS project-id

#### Client
* Request prerequisites

Name|Validation|Default|Info
---|---|---|---
git\_repo\_url|non blank string||The string of the go import path e.g "go.opencensus.io/exporter" or "go.opencensus.io/..."
public|boolean|false|If set to true, creates benchmarks that can be accessible by anyone with the URL 
alert\_emails|array of strings||A required listing of people to email if results change or are run for the first time for example ["foo@bar.com", "baz@example.org"]


Example request:
```shell
curl -X POST $URL/benchmark --data \
'{
  "git_repo_url":"go.opencensus.io/exporter", 
  "public":true,
  "alert_emails":["emm.odeke@gmail.com", "emmanuel@orijtech.com"]
}'
```
