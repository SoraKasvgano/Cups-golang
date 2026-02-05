package config

import (
	"bufio"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cupsgolang/internal/model"
	"cupsgolang/internal/store"
)

type legacyPrinter struct {
	name      string
	info      string
	location  string
	deviceURI string
	geo       string
	org       string
	orgUnit   string
}

type legacyClass struct {
	name     string
	info     string
	location string
	members  []string
}

func SyncFromConf(ctx context.Context, confDir string, st *store.Store) error {
	if confDir == "" || st == nil {
		return nil
	}
	_ = os.MkdirAll(confDir, 0755)
	printers, _ := parsePrintersConf(filepath.Join(confDir, "printers.conf"))
	classes, _ := parseClassesConf(filepath.Join(confDir, "classes.conf"))

	return st.WithTx(ctx, false, func(tx *sql.Tx) error {
		for _, p := range printers {
			printer, _ := st.UpsertPrinter(ctx, tx, p.name, p.deviceURI, p.location, p.info, true)
			var geo *string
			var org *string
			var orgUnit *string
			if p.geo != "" {
				geo = &p.geo
			}
			if p.org != "" {
				org = &p.org
			}
			if p.orgUnit != "" {
				orgUnit = &p.orgUnit
			}
			if geo != nil || org != nil || orgUnit != nil {
				_ = st.UpdatePrinterAttributes(ctx, tx, printer.ID, nil, nil, geo, org, orgUnit)
			}
		}
		for _, c := range classes {
			memberIDs := make([]int64, 0, len(c.members))
			for _, m := range c.members {
				if printer, err := st.GetPrinterByName(ctx, tx, m); err == nil {
					memberIDs = append(memberIDs, printer.ID)
				}
			}
			_, _ = st.UpsertClass(ctx, tx, c.name, c.location, c.info, true, memberIDs)
		}
		return nil
	})
}

func SyncToConf(ctx context.Context, confDir string, st *store.Store) error {
	if confDir == "" || st == nil {
		return nil
	}
	_ = os.MkdirAll(confDir, 0755)
	return st.WithTx(ctx, true, func(tx *sql.Tx) error {
		printers, err := st.ListPrinters(ctx, tx)
		if err != nil {
			return err
		}
		classes, err := st.ListClasses(ctx, tx)
		if err != nil {
			return err
		}
		if err := writePrintersConf(filepath.Join(confDir, "printers.conf"), printers); err != nil {
			return err
		}
		if err := writeClassesConf(filepath.Join(confDir, "classes.conf"), classes, st, tx); err != nil {
			return err
		}
		return nil
	})
}

func parsePrintersConf(path string) ([]legacyPrinter, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []legacyPrinter
	var cur *legacyPrinter
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "<Printer ") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "<Printer "), ">")
			cur = &legacyPrinter{name: name}
			continue
		}
		if line == "</Printer>" {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = nil
			continue
		}
		if cur == nil || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "Info":
			cur.info = parts[1]
		case "Location":
			cur.location = parts[1]
		case "DeviceURI":
			cur.deviceURI = parts[1]
		case "GeoLocation":
			cur.geo = parts[1]
		case "Organization":
			cur.org = parts[1]
		case "OrganizationalUnit":
			cur.orgUnit = parts[1]
		}
	}
	return out, sc.Err()
}

func parseClassesConf(path string) ([]legacyClass, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []legacyClass
	var cur *legacyClass
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "<Class ") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "<Class "), ">")
			cur = &legacyClass{name: name}
			continue
		}
		if line == "</Class>" {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = nil
			continue
		}
		if cur == nil || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "Info":
			cur.info = parts[1]
		case "Location":
			cur.location = parts[1]
		case "Member":
			cur.members = append(cur.members, parts[1])
		}
	}
	return out, sc.Err()
}

func writePrintersConf(path string, printers []model.Printer) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	for _, p := range printers {
		_, _ = w.WriteString("<Printer " + p.Name + ">\n")
		if p.Info != "" {
			_, _ = w.WriteString("Info " + p.Info + "\n")
		}
		if p.Location != "" {
			_, _ = w.WriteString("Location " + p.Location + "\n")
		}
		if p.URI != "" {
			_, _ = w.WriteString("DeviceURI " + p.URI + "\n")
		}
		if p.Geo != "" {
			_, _ = w.WriteString("GeoLocation " + p.Geo + "\n")
		}
		if p.Org != "" {
			_, _ = w.WriteString("Organization " + p.Org + "\n")
		}
		if p.OrgUnit != "" {
			_, _ = w.WriteString("OrganizationalUnit " + p.OrgUnit + "\n")
		}
		_, _ = w.WriteString("State " + strconv.Itoa(p.State) + "\n")
		_, _ = w.WriteString("</Printer>\n")
	}
	return nil
}

func writeClassesConf(path string, classes []model.Class, st *store.Store, tx *sql.Tx) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	for _, c := range classes {
		_, _ = w.WriteString("<Class " + c.Name + ">\n")
		if c.Info != "" {
			_, _ = w.WriteString("Info " + c.Info + "\n")
		}
		if c.Location != "" {
			_, _ = w.WriteString("Location " + c.Location + "\n")
		}
		members, _ := st.ListClassMembers(context.Background(), tx, c.ID)
		for _, m := range members {
			_, _ = w.WriteString("Member " + m.Name + "\n")
		}
		_, _ = w.WriteString("</Class>\n")
	}
	return nil
}

func SyncLoop(ctx context.Context, confDir string, st *store.Store) {
	ticker := time.NewTicker(15 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = SyncToConf(ctx, confDir, st)
			}
		}
	}()
}
