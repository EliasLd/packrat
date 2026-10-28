.PHONY: build test lint fmt tidy clean

build:
	go build -o build/packrat ./cmd/packrat

test:
	go test ./... -race

lint:
	golangci-lint run

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

clean:
	rm -rf build