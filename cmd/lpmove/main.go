package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "lpmove: usage: lpmove job destination")
		os.Exit(1)
	}
	jobID := parseJobID(os.Args[1])
	if jobID == 0 {
		fmt.Fprintln(os.Stderr, "lpmove: invalid job id")
		os.Exit(1)
	}
	dest := os.Args[2]

	client := cupsclient.NewFromEnv()
	destURI := dest
	if !strings.Contains(dest, "://") {
		destURI = client.PrinterURI(dest)
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsMoveJob, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobID)))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(destURI)))

	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lpmove:", err)
		os.Exit(1)
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		fmt.Fprintln(os.Stderr, "lpmove:", goipp.Status(resp.Code))
		os.Exit(1)
	}
}

func parseJobID(arg string) int {
	if strings.Contains(arg, "-") {
		parts := strings.Split(arg, "-")
		arg = parts[len(parts)-1]
	}
	n, _ := strconv.Atoi(arg)
	return n
}
