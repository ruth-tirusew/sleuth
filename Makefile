.PHONY: build test e2e e2e-race check
build:
	go build -o sleuth ./cmd/sleuth

# unit tests, the race detector, and the end-to-end suite
test:
	go vet ./...
	go test -race ./...

# end-to-end only: builds the real binary and runs it against an in-process service
e2e:
	go test -count=1 ./e2e

# the same, with the program under test built with the race detector
e2e-race:
	SLEUTH_E2E_RACE=1 go test -count=1 ./e2e

check: test e2e-race
