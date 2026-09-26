package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"universal-bypass-tool/transport/yandex"
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: volga-probe <classify|auth|ws> --url <public-yandex-document-url>\n")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	docURL := fs.String("url", "", "Public Yandex document URL")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if *docURL == "" {
		usage()
		os.Exit(2)
	}

	var result yandex.VolgaProbeResult
	switch mode {
	case "classify":
		result = yandex.ProbeVolgaClassify(*docURL)
	case "auth":
		result = yandex.ProbeVolgaAuth(*docURL)
	case "ws":
		result = yandex.ProbeVolgaWS(*docURL)
	default:
		usage()
		os.Exit(2)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if result.Error != "" {
		os.Exit(1)
	}
}
