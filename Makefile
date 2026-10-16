IMAGE ?= ghcr.io/nobloat/up
TAG ?= $(or $(shell git describe --tags --exact-match 2>/dev/null),main)

.PHONY: build test run mailpit package push screenshots clean

build:
	CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags="-s -w -X main.release=$(TAG)" -o up .
	@du -h up | cut -f1 | xargs echo "binary size:"

test:
	go vet ./... && go test -race ./...

run: build
	./up -config config

mailpit: # local SMTP sink for the sample config's demo-mail notifier, mails at http://localhost:8025
	docker start nobloat-up-mailpit 2>/dev/null || docker run -d --rm --name nobloat-up-mailpit -p 1025:1025 -p 8025:8025 axllent/mailpit:v1.27

package:
	docker build -t $(IMAGE):$(TAG) .
	@docker image ls $(IMAGE):$(TAG) --format "image size: {{.Size}}"

push: package
	docker push $(IMAGE):$(TAG)

screenshots:
	./screenshots.sh

clean:
	go clean ./...
	rm -rf up data screenshots/*/  # data: audit logs from make run (config/up.conf data_dir)
