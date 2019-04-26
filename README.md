# AWS ROUTE53 BACKUP LAMBDA

Snapshots Route53 records for a given domain and uploads them to S3. 

## Deployment

### Pre-requisites
* AWS Account credentials
* S3 bucket to upload backup
* Route53 Hosted Zone name or Id

### Deployment

* Download and run installer for nodejs from [here](https://nodejs.org/en/)
* Download and run installer for golang from [here](https://golang.org/dl/)
* Download and run installer for git https://git-scm.com/downloads
* Open Terminal/CommandPrompt and run the following commands

```bash
# install serverless
npm install -g serverless

# install dep (golang dependency management tool)
brew install dep # MacOS only For Windows download from https://go.equinox.io/github.com/golang/dep/cmd/dep and place it in path 

# Get the source code
git clone https://github.com/bob2build/route53-backup-lambda.git

# Update the serverless.yml file with the s3 bucket name and route53 hosted zone details

# build lamdba function
dep ensure -v
GOOS=linux go build -ldflags="-s -w" -o bin/operator operator/main.go

# deploy to AWS
sls deploy -v
``` 

## TODO
* Remove cli53 dependency and use custom format to backup route53 records
* Enable notification message to contain record differences between backups 
