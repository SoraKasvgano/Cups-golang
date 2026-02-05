package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/cupsclient"
)

type options struct {
	printer     string
	deviceURI   string
	description string
	location    string
	ppdFile     string
	ppdName     string
	deleteName  string
	defaultName string
	enable      bool
	classAdd    string
	classRemove string
}

func main() {
	opts := parseArgs(os.Args[1:])
	client := cupsclient.NewFromEnv()

	if opts.deleteName != "" {
		if err := deleteDestination(client, opts.deleteName); err != nil {
			fail(err)
		}
		return
	}

	if opts.printer != "" || opts.deviceURI != "" || opts.description != "" || opts.location != "" {
		if opts.printer == "" {
			fail(fmt.Errorf("missing -p printer"))
		}
		if err := addModifyPrinter(client, opts); err != nil {
			fail(err)
		}
	}

	if opts.classAdd != "" || opts.classRemove != "" {
		if opts.printer == "" {
			fail(fmt.Errorf("missing -p printer for class operation"))
		}
		if err := updateClassMembers(client, opts.printer, opts.classAdd, opts.classRemove); err != nil {
			fail(err)
		}
	}

	if opts.enable && opts.printer != "" {
		if err := resumePrinter(client, opts.printer); err != nil {
			fail(err)
		}
		if err := acceptPrinter(client, opts.printer); err != nil {
			fail(err)
		}
	}

	if opts.defaultName != "" {
		if err := setDefault(client, opts.defaultName); err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lpadmin:", err)
	os.Exit(1)
}

func parseArgs(args []string) options {
	opts := options{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p":
			if i+1 < len(args) {
				i++
				opts.printer = args[i]
			}
		case "-v":
			if i+1 < len(args) {
				i++
				opts.deviceURI = args[i]
			}
		case "-D":
			if i+1 < len(args) {
				i++
				opts.description = args[i]
			}
		case "-L":
			if i+1 < len(args) {
				i++
				opts.location = args[i]
			}
		case "-x":
			if i+1 < len(args) {
				i++
				opts.deleteName = args[i]
			}
		case "-P":
			if i+1 < len(args) {
				i++
				opts.ppdFile = args[i]
			}
		case "-m":
			if i+1 < len(args) {
				i++
				opts.ppdName = args[i]
			}
		case "-d":
			if i+1 < len(args) {
				i++
				opts.defaultName = args[i]
			}
		case "-E":
			opts.enable = true
		case "-c":
			if i+1 < len(args) {
				i++
				opts.classAdd = args[i]
			}
		case "-r":
			if i+1 < len(args) {
				i++
				opts.classRemove = args[i]
			}
		}
	}
	return opts
}

func addModifyPrinter(client *cupsclient.Client, opts options) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyPrinter, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(opts.printer))))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(opts.printer)))

	if opts.deviceURI != "" {
		req.Printer.Add(goipp.MakeAttribute("device-uri", goipp.TagURI, goipp.String(opts.deviceURI)))
	}
	if opts.ppdName != "" {
		req.Printer.Add(goipp.MakeAttribute("ppd-name", goipp.TagName, goipp.String(opts.ppdName)))
	}
	if opts.description != "" {
		req.Printer.Add(goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String(opts.description)))
	}
	if opts.location != "" {
		req.Printer.Add(goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String(opts.location)))
	}
	var payload *os.File
	if opts.ppdFile != "" {
		f, err := os.Open(opts.ppdFile)
		if err != nil {
			return err
		}
		payload = f
		defer payload.Close()
	}
	resp, err := client.Send(context.Background(), req, payload)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func deleteDestination(client *cupsclient.Client, name string) error {
	if name == "" {
		return nil
	}
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsDeletePrinter, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(name)))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	resp, err := client.Send(context.Background(), req, nil)
	if err == nil && goipp.Status(resp.Code) < goipp.StatusRedirectionOtherSite {
		return nil
	}

	// Try delete class if printer delete failed.
	req = goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsDeleteClass, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(name)))
	resp, err = client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func setDefault(client *cupsclient.Client, name string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsSetDefault, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(name)))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func resumePrinter(client *cupsclient.Client, name string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpResumePrinter, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func acceptPrinter(client *cupsclient.Client, name string) error {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAcceptJobs, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(client.PrinterURI(name))))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func updateClassMembers(client *cupsclient.Client, printerName, classAdd, classRemove string) error {
	className := classAdd
	if className == "" {
		className = classRemove
	}
	if className == "" {
		return nil
	}

	members, err := fetchClassMembers(client, className)
	if err != nil {
		return err
	}

	memberSet := map[string]bool{}
	for _, m := range members {
		memberSet[m] = true
	}
	if classAdd != "" {
		memberSet[printerName] = true
	}
	if classRemove != "" {
		delete(memberSet, printerName)
	}

	memberList := make([]goipp.Value, 0, len(memberSet))
	for name := range memberSet {
		memberList = append(memberList, goipp.String(client.PrinterURI(name)))
	}

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsAddModifyClass, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(className)))
	if len(memberList) > 0 {
		req.Printer.Add(goipp.MakeAttr("member-uris", goipp.TagURI, memberList[0], memberList[1:]...))
	}
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusRedirectionOtherSite {
		return fmt.Errorf("%s", goipp.Status(resp.Code))
	}
	return nil
}

func fetchClassMembers(client *cupsclient.Client, className string) ([]string, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCupsGetClasses, uint32(time.Now().UnixNano()))
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	resp, err := client.Send(context.Background(), req, nil)
	if err != nil {
		return nil, err
	}
	for _, g := range resp.Groups {
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		name := findAttr(g.Attrs, "printer-name")
		if !strings.EqualFold(name, className) {
			continue
		}
		return attrValues(g.Attrs, "member-uris"), nil
	}
	return nil, nil
}

func findAttr(attrs goipp.Attributes, name string) string {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return ""
}

func attrValues(attrs goipp.Attributes, name string) []string {
	out := []string{}
	for _, a := range attrs {
		if a.Name != name {
			continue
		}
		for _, v := range a.Values {
			out = append(out, v.V.String())
		}
	}
	return out
}
