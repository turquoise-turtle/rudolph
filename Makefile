PKG = github.com/airbnb/rudolph
VERSION := $(shell git describe --tags --always)
DOCS_DIR ?= ./docs
RUDOLPH_API_DEPLOYMENT_ZIP_PATH = $(PWD)/build/package/api_deployment.zip
RUDOLPH_API_AUTHORIZER_DEPLOYMENT_ZIP_PATH = $(PWD)/build/package/api_authorizer_deployment.zip
RUDOLPH_RANKER_DEPLOYMENT_ZIP_PATH = $(PWD)/build/package/ranker_deployment.zip
TERRAFORM_DEPLOYMENTS_DIR = $(PWD)/deployments/environments
TF_DEFAULT_FLAGS = --var lambda_api_zip="$(RUDOLPH_API_DEPLOYMENT_ZIP_PATH)" --var lambda_authorizer_zip="$(RUDOLPH_API_AUTHORIZER_DEPLOYMENT_ZIP_PATH)" --var lambda_ranker_zip="$(RUDOLPH_RANKER_DEPLOYMENT_ZIP_PATH)"
LDFLAGS=-ldflags="-X main.version=$(VERSION)"

# Check to ensure the prefix is being passed in as an arg like `ENV=<YOUR_ENVIRONMENT>`
# or is set using environment variables like `export ENV=<YOUR_ENVIRONMENT>`
#
.check-args:
ifndef ENV
	$(error ENV is not set. please use `ENV=<YOUR_ENV> make deploy` or `export ENV=<YOUR_ENV>.`)
endif

TERRAFORM_DEPLOYMENTS_ENV_DIR = $(TERRAFORM_DEPLOYMENTS_DIR)/$(ENV)

# make init-env copies the example environment once, so an environment created before a release that added a module
# variable lacks it, and Terraform plans without the setting and only warns about the value in config.auto.tfvars.json.
# Stops plan and apply until the environment has every variable and module argument the example has and declares every
# value its .tfvars.json files set.
#
.check-env: .check-args
	go run ./cmd/envcheck $(TERRAFORM_DEPLOYMENTS_ENV_DIR)

# Default target for `make`; Builds all Rudolph infrastructure and deploys the source code.
#
#
deploy: .check-args .check-env test build tf-init justdeploy

# Performs deploy without running tests or rebuilding the application. This is useful if you just
# recently already did a build but did not make any code changes, and don't want to repeat the whole
# process, but you should generally AVOID using this make target unless you know what you're doing.
justdeploy: .check-args .check-env
	terraform -chdir=$(TERRAFORM_DEPLOYMENTS_ENV_DIR) apply $(TF_DEFAULT_FLAGS)

# Equivalent of `terraform plan` in the desired environment directory.
#
#
plan: .check-args .check-env
	terraform -chdir=$(TERRAFORM_DEPLOYMENTS_ENV_DIR) plan $(TF_DEFAULT_FLAGS)

# Equivalent to a `terraform init` in the desired environment directory.
# Required upon first deployment and when the tf state drifts too far from the local cached state.
#
tf-init: .check-args
	terraform -chdir=$(TERRAFORM_DEPLOYMENTS_ENV_DIR) init

# Destroys the current deployment
#
#
destroy: tf-init
	terraform -chdir=$(TERRAFORM_DEPLOYMENTS_ENV_DIR) destroy $(TF_DEFAULT_FLAGS)

# When the API Gateway deployment fails to re-deploy (for whatever reason), you can use this
# command to force a redeployment
#
force-api-redeploy: .check-args .check-env
	terraform -chdir=$(TERRAFORM_DEPLOYMENTS_ENV_DIR) taint module.santa_api.aws_api_gateway_deployment.api_deployment
	terraform -chdir=$(TERRAFORM_DEPLOYMENTS_ENV_DIR) apply --target=module.santa_api.aws_api_gateway_deployment.api_deployment $(TF_DEFAULT_FLAGS)

# Install dependencies
#
#
deps:
	go mod download
	go mod tidy

# Checks the Go dependencies: known vulnerabilities that affect the code (fails if any), then every module in the
# build graph that has a newer version or is deprecated (for information; it does not fail).
#
GOVULNCHECK_VERSION = v1.9.0
deps-check:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
	go list -m -u -f '{{if or .Update .Deprecated}}{{.Path}} {{.Version}}{{with .Update}} (newer: {{.Version}}){{end}}{{with .Deprecated}} (deprecated: {{.}}){{end}}{{end}}' \
		$$(go list -deps -test -tags integration -f '{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}' ./... | sort -u)
# The listing covers only modules that provide a package the build or the tests compile. The rest of the module graph
# (modules that other modules' go.mod files merely mention) is never built, and go mod tidy drops any pin of it.

# Compiles golang source into binaries
# We use linux amd64 binaries so AWS Lambda can run them
#
build: deps
	@sh -c "'$(CURDIR)/scripts/build.sh'"

# run all golang unit tests
#
#
test:
	go test -cover -v ./...

# Run unit and integration tests. Integration tests need Java + DynamoDB Local (installed by
# scripts/setup_test_deps.sh) and terraform on PATH.
#
test-integration:
	@sh -c "'$(CURDIR)/scripts/setup_test_deps.sh'"
	go test -tags integration -count=1 ./...

# Initializes the environment directory and files
#
#
init-env: .check-args
	@sh -c "'$(CURDIR)/scripts/new_env.sh' $(ENV)"

#
#
#
.PHONY: .check-args .check-env deploy tf-init plan destroy deps deps-check build test force-api-redeploy
