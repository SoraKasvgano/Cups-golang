package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func main() {
	flag.Parse()
	printers := flag.Args()
	if len(printers) == 0 {
		printers = []string{"Default"}
	}
	client := cupsclient.NewFromEnv()
	for _, p := range printers {
		if err := releaseHeldJobs(client, p); err != nil {
			fmt.Fprintln(os.Stderr, "cupsaccept:", err)
			os.Exit(1)
		}
	}
}

func releaseHeldJobs(client *cupsclient.Client, name string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAcceptJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	_, err := client.Send(context.Background(), req, nil)
	return err
}
