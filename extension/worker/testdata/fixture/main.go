// Command fixture serves the shared native fixture through the Go authoring
// adapter for host integration tests.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/MMinasyan/lightcode/extension/worker"
	fixture "github.com/MMinasyan/lightcode/extension/worker/testdata/fixture/native"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-declaration" {
		encoded, err := json.Marshal(worker.NewDeclaration(fixture.Declaration()))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if _, err := os.Stdout.Write(encoded); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := worker.Serve(context.Background(), fixture.Native(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	fixture.LogEnvironment()
}
