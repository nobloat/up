IMAGE ?= ghcr.io/nobloat/up
TAG ?= $(or $(shell git describe --tags --exact-match 2>/dev/null),main)
UI ?= ../ui

.PHONY: build test run nomail package push screenshots ui clean

build:
	CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags="-s -w -X main.release=$(TAG)" -o up .
	@du -h up | cut -f1 | xargs echo "binary size:"

test:
	go vet ./... && go test -race ./...

run: build
	./up -config config

nomail: # local SMTP + IMAP sink (../nomail) for the sample config's demo-mail notifier, mails at http://localhost:8025
	$(MAKE) -C ../nomail docker

package:
	docker build -t $(IMAGE):$(TAG) .
	@docker image ls $(IMAGE):$(TAG) --format "image size: {{.Size}}"

push: package
	docker push $(IMAGE):$(TAG)

ui: # the shared look from nobloat/ui (../ui), copied unchanged; app styles live in web/static/app.css, app code in app.js
	cp $(UI)/ui.css $(UI)/ui.js web/static/
	cp $(UI)/partials.html web/
	cp $(UI)/i18n/en.json $(UI)/i18n/de.json i18n/ui/

screenshots:
	./screenshots.sh

clean:
	go clean ./...
	rm -rf up data screenshots/*/  # data: audit logs from make run (config/up.conf data_dir)
