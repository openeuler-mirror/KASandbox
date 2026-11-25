ENV := $(shell cat .last_used_env || echo "not-set")
ENV_FILE := .env.${ENV}
PROVIDER ?= gcp

-include ${ENV_FILE}

AWS_BUCKET_PREFIX ?= $(PREFIX)$(AWS_ACCOUNT_ID)-
GCP_BUCKET_PREFIX ?= $(GCP_PROJECT_ID)-

.PHONY: provider-login
provider-login:
	$(MAKE) -C iac/provider-$(PROVIDER) provider-login

.PHONY: init
init:
	./scripts/confirm.sh $(TERRAFORM_ENVIRONMENT)
	$(MAKE) -C iac/provider-$(PROVIDER) init

# Setup production environment variables, this is used only for E2B.dev production
# Uses Infisical CLI to read secrets from Infisical Vault
# To update them, use the Infisical UI directly
# On a first use, you need to run `infisical login` and `infisical init`
.PHONY: download-prod-env
download-prod-env:
	@  ./scripts/download-prod-env.sh ${ENV}

.PHONY: plan
plan:
	$(MAKE) -C iac/provider-$(PROVIDER) plan

# Deploy all jobs in Nomad
.PHONY: plan-only-jobs
plan-only-jobs:
	$(MAKE) -C iac/provider-$(PROVIDER) plan-only-jobs

# Deploy a specific job name in Nomad
# When job name is specified, all '-' are replaced with '_' in the job name
.PHONY: plan-only-jobs/%
plan-only-jobs/%:
	$(MAKE) -C iac/provider-$(PROVIDER) plan-only-jobs/$(subst -,_,$(notdir $@))

.PHONY: plan-without-jobs
plan-without-jobs:
	$(MAKE) -C iac/provider-$(PROVIDER) plan-without-jobs

.PHONY: apply-init
apply-init:
	$(MAKE) -C iac/provider-$(PROVIDER) apply-init

.PHONY: apply
apply:
	./scripts/confirm.sh $(TERRAFORM_ENVIRONMENT)
	$(MAKE) -C iac/provider-$(PROVIDER) apply

# Shortcut to importing resources into Terraform state (e.g. after creating resources manually or switching between different branches for the same environment)
.PHONY: import
import:
	./scripts/confirm.sh $(TERRAFORM_ENVIRONMENT)
	$(MAKE) -C iac/provider-$(PROVIDER) import

.PHONY: version
version:
	./scripts/increment-version.sh

.PHONY: copy-public-builds
copy-public-builds:
ifeq ($(PROVIDER),aws)
	mkdir -p ./.kernels
	mkdir -p ./.firecrackers
	gsutil -m cp -r gs://e2b-prod-public-builds/kernels/* ./.kernels/
	gsutil -m cp -r gs://e2b-prod-public-builds/firecrackers/* ./.firecrackers/
	aws s3 cp ./.kernels/ s3://${AWS_BUCKET_PREFIX}fc-kernels/ --recursive --profile ${AWS_PROFILE}
	aws s3 cp ./.firecrackers/ s3://${AWS_BUCKET_PREFIX}fc-versions/ --recursive --profile ${AWS_PROFILE}
	rm -rf ./.kernels
	rm -rf ./.firecrackers
else
	gsutil cp -r gs://e2b-prod-public-builds/kernels/* gs://$(GCP_BUCKET_PREFIX)fc-kernels/
	gsutil cp -r gs://e2b-prod-public-builds/firecrackers/* gs://$(GCP_BUCKET_PREFIX)fc-versions/
endif

.PHONY: set-env
set-env:
	@ touch .last_used_env
	@ echo $(ENV) > .last_used_env
	@ . ./${ENV_FILE}

.PHONY: switch-env
switch-env:
	@ printf "Switching from `tput setaf 1``tput bold`$(shell cat .last_used_env)`tput sgr0` to `tput setaf 2``tput bold`$(ENV)`tput sgr0`\n\n"
	$(MAKE) set-env ENV=$(ENV)
	make -C iac/provider-$(PROVIDER) switch

.PHONY: setup-ssh
setup-ssh:
	@ printf "Setting up SSH for env: `tput setaf 2``tput bold`$(ENV)`tput sgr0`\n"
	@ gcloud compute config-ssh --remove
	@ gcloud compute config-ssh --project $(GCP_PROJECT_ID) --quiet
	@ printf "SSH setup complete\n"

.PHONY: test
test:
	cd cri-multiplex && go test ./...
	cd e2b-webhook && go test ./...

.PHONY: fmt
fmt:
	golangci-lint fmt
	terraform fmt -recursive

.PHONY: lint
lint:
	cd cri-multiplex && golangci-lint run ./...
	cd e2b-webhook && golangci-lint run ./...

.PHONY: tidy
tidy:
	@scripts/golang-dependencies-integrity.sh
