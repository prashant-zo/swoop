.PHONY: build install test clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o swoop .

install:
	./install.sh

test:
	go vet ./... && go test ./...

clean:
	rm -f swoop
