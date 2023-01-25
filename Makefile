.PHONY: test install cover

all: install

test:
	go test -v github.com/gokrazy/imaged/...

cover:
	go test \
		-coverpkg="$(shell go list ./... | paste -sd ,)" \
		-coverprofile=/tmp/cover.profile \
		-v \
		github.com/gokrazy/imaged/...

install:
	go install github.com/gokrazy/imaged/cmd/...
