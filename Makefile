.PHONY: build fmt vet check clean

build:
	go build -o read-once .

fmt:
	gofmt -w main.go

vet:
	go vet ./...

check: fmt vet
	@gofmt -l . | grep . && echo "gofmt: files need formatting" && exit 1 || true
	@echo "All checks passed."

clean:
	rm -f read-once read-once.exe
