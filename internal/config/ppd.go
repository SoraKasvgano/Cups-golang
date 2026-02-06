package config

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type PPD struct {
	NickName          string
	Model             string
	Options           map[string][]string
	Defaults          map[string]string
	Make              string
	ColorDevice       bool
	DefaultResolution string
	Resolutions       []string
	DefaultColorSpace string
	ColorSpaces       []string
	Protocols         []string
	PortMonitors      []string
	Filters           []PPDFilter
	Constraints       []PPDConstraint
	OrderDependencies []PPDOrderDependency
	Groups            []PPDGroup
	OptionDetails     map[string]*PPDOption
	PageSizes         map[string]PPDPageSize
	HWMargins         [4]int
	CustomMinSize     [2]int
	CustomMaxSize     [2]int
	ChargeInfoURI     string
	JobAccountID      bool
	JobAccountingUser bool
	JobPassword       string
	Mandatory         []string
	MaxCopies         int
	ManualCopies      bool
	DeviceID          string
	Throughput        int
	Presets           []PPDPrinterPreset
	APICADriver       bool
	APScannerOnly     bool
}

type PPDGroup struct {
	Name    string
	Text    string
	Options []*PPDOption
}

type PPDOption struct {
	Keyword      string
	Text         string
	UI           string
	Group        string
	Choices      []PPDChoice
	Default      string
	Custom       bool
	CustomParams []PPDCustomParam
}

type PPDChoice struct {
	Choice string
	Text   string
}

type PPDPrinterPreset struct {
	Name    string
	Text    string
	Options []PPDPresetOption
}

type PPDPresetOption struct {
	Option string
	Value  string
}

type PPDPageSize struct {
	Name   string
	Width  int
	Length int
	Left   int
	Bottom int
	Right  int
	Top    int
}

type PPDCustomParam struct {
	Name  string
	Text  string
	Type  string
	Order int
	Min   float64
	Max   float64
	Range bool
}

type PPDFilter struct {
	Source  string
	Dest    string
	Cost    int
	Program string
}

type PPDConstraint struct {
	Option1 string
	Choice1 string
	Option2 string
	Choice2 string
}

type PPDOrderDependency struct {
	Order   int
	Section string
	Option  string
}

func LoadPPD(path string) (*PPD, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ppd := &PPD{
		Options:       map[string][]string{},
		Defaults:      map[string]string{},
		OptionDetails: map[string]*PPDOption{},
		PageSizes:     map[string]PPDPageSize{},
	}
	groupMap := map[string]*PPDGroup{}
	var currentGroup *PPDGroup
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "*HWMargins:") {
			if margins, ok := parsePPDHWMargins(line); ok {
				ppd.HWMargins = margins
			}
			continue
		}
		if strings.HasPrefix(line, "*UIConstraints:") || strings.HasPrefix(line, "*NonUIConstraints:") {
			if c, ok := parsePPDConstraint(line); ok {
				ppd.Constraints = append(ppd.Constraints, c)
			}
			continue
		}
		if strings.HasPrefix(line, "*OrderDependency:") {
			if dep, ok := parsePPDOrderDependency(line); ok {
				ppd.OrderDependencies = append(ppd.OrderDependencies, dep)
			}
			continue
		}
		if strings.HasPrefix(line, "*NickName:") {
			ppd.NickName = strings.Trim(line[len("*NickName:"):], " \"")
		}
		if strings.HasPrefix(line, "*ModelName:") {
			ppd.Model = strings.Trim(line[len("*ModelName:"):], " \"")
		}
		if strings.HasPrefix(line, "*Manufacturer:") {
			ppd.Make = strings.Trim(line[len("*Manufacturer:"):], " \"")
		}
		if strings.HasPrefix(line, "*ColorDevice:") {
			val := strings.TrimSpace(strings.Trim(line[len("*ColorDevice:"):], " \""))
			ppd.ColorDevice = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "yes")
		}
		if strings.HasPrefix(line, "*cupsChargeInfoURI:") {
			ppd.ChargeInfoURI = strings.TrimSpace(strings.Trim(line[len("*cupsChargeInfoURI:"):], " \""))
		}
		if strings.HasPrefix(line, "*cupsJobAccountId:") {
			val := strings.TrimSpace(strings.Trim(line[len("*cupsJobAccountId:"):], " \""))
			ppd.JobAccountID = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "yes")
		}
		if strings.HasPrefix(line, "*cupsJobAccountingUserId:") {
			val := strings.TrimSpace(strings.Trim(line[len("*cupsJobAccountingUserId:"):], " \""))
			ppd.JobAccountingUser = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "yes")
		}
		if strings.HasPrefix(line, "*cupsJobPassword:") {
			ppd.JobPassword = strings.TrimSpace(strings.Trim(line[len("*cupsJobPassword:"):], " \""))
		}
		if strings.HasPrefix(line, "*cupsMaxCopies:") {
			val := strings.TrimSpace(strings.Trim(line[len("*cupsMaxCopies:"):], " \""))
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				ppd.MaxCopies = n
			}
		}
		if strings.HasPrefix(line, "*ManualCopies:") {
			val := strings.TrimSpace(strings.Trim(line[len("*ManualCopies:"):], " \""))
			ppd.ManualCopies = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "yes")
		}
		if strings.HasPrefix(line, "*cupsMandatory:") {
			val := strings.TrimSpace(strings.Trim(line[len("*cupsMandatory:"):], " \""))
			if val != "" {
				ppd.Mandatory = append(ppd.Mandatory, splitPPDList(val)...)
			}
		}
		if strings.HasPrefix(line, "*DefaultResolution:") {
			ppd.DefaultResolution = strings.TrimSpace(strings.Trim(line[len("*DefaultResolution:"):], " \""))
		}
		if strings.HasPrefix(line, "*DefaultColorSpace:") {
			ppd.DefaultColorSpace = strings.TrimSpace(strings.Trim(line[len("*DefaultColorSpace:"):], " \""))
		}
		if strings.HasPrefix(line, "*cupsFilter2:") {
			if filter, ok := parsePPDFilterLine(line, true); ok {
				ppd.Filters = append(ppd.Filters, filter)
			}
			continue
		}
		if strings.HasPrefix(line, "*cupsFilter:") {
			if filter, ok := parsePPDFilterLine(line, false); ok {
				ppd.Filters = append(ppd.Filters, filter)
			}
			continue
		}
		if strings.HasPrefix(line, "*Protocols:") {
			val := strings.TrimSpace(strings.Trim(line[len("*Protocols:"):], " \""))
			if val != "" {
				for _, token := range splitPPDList(val) {
					if token == "" {
						continue
					}
					seen := false
					for _, existing := range ppd.Protocols {
						if strings.EqualFold(existing, token) {
							seen = true
							break
						}
					}
					if !seen {
						ppd.Protocols = append(ppd.Protocols, token)
					}
				}
			}
		}
		if strings.HasPrefix(line, "*cupsPortMonitor") {
			if idx := strings.Index(line, ":"); idx != -1 {
				val := strings.TrimSpace(line[idx+1:])
				val = strings.Trim(val, " \"")
				if val != "" {
					seen := false
					for _, existing := range ppd.PortMonitors {
						if strings.EqualFold(existing, val) {
							seen = true
							break
						}
					}
					if !seen {
						ppd.PortMonitors = append(ppd.PortMonitors, val)
					}
				}
			}
		}
		if strings.HasPrefix(line, "*APICADriver:") {
			val := strings.TrimSpace(strings.Trim(line[len("*APICADriver:"):], " \""))
			ppd.APICADriver = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "yes")
			continue
		}
		if strings.HasPrefix(line, "*APScannerOnly:") {
			val := strings.TrimSpace(strings.Trim(line[len("*APScannerOnly:"):], " \""))
			ppd.APScannerOnly = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "yes")
			continue
		}
		if strings.HasPrefix(line, "*1284DeviceId:") || strings.HasPrefix(line, "*1284DeviceID:") {
			val := strings.TrimSpace(line[strings.Index(line, ":")+1:])
			val = strings.Trim(val, " \"")
			if val != "" && ppd.DeviceID == "" {
				ppd.DeviceID = val
			}
			continue
		}
		if strings.HasPrefix(line, "*Throughput:") {
			val := strings.TrimSpace(line[len("*Throughput:"):])
			val = strings.Trim(val, " \"")
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				ppd.Throughput = n
			} else if f, err := strconv.ParseFloat(val, 64); err == nil && f > 0 {
				ppd.Throughput = int(math.Round(f))
			}
			continue
		}
		if strings.HasPrefix(line, "*APPrinterPreset ") {
			if preset, ok := parsePPDPrinterPreset(line); ok {
				ppd.Presets = append(ppd.Presets, preset)
			}
			continue
		}
		if strings.HasPrefix(line, "*OpenGroup:") {
			group := strings.TrimSpace(line[len("*OpenGroup:"):])
			name, text := splitPPDLabel(group)
			if name == "" {
				name = "General"
			}
			if text == "" {
				text = name
			}
			if existing, ok := groupMap[name]; ok {
				currentGroup = existing
			} else {
				g := &PPDGroup{Name: name, Text: text}
				ppd.Groups = append(ppd.Groups, *g)
				groupMap[name] = &ppd.Groups[len(ppd.Groups)-1]
				currentGroup = groupMap[name]
			}
			continue
		}
		if strings.HasPrefix(line, "*CloseGroup:") {
			currentGroup = nil
			continue
		}
		if strings.HasPrefix(line, "*OpenUI ") {
			parts := strings.SplitN(line, ":", 2)
			left := strings.TrimSpace(strings.TrimPrefix(parts[0], "*OpenUI"))
			ui := ""
			if len(parts) == 2 {
				ui = strings.TrimSpace(parts[1])
			}
			left = strings.TrimSpace(left)
			left = strings.TrimPrefix(left, "*")
			key, text := splitPPDLabel(left)
			if key == "" {
				continue
			}
			ppd.Options[key] = []string{}
			opt := &PPDOption{
				Keyword: key,
				Text:    text,
				UI:      normalizePPDUI(ui),
			}
			if opt.Text == "" {
				opt.Text = key
			}
			if currentGroup != nil {
				opt.Group = currentGroup.Name
				currentGroup.Options = append(currentGroup.Options, opt)
			} else {
				group := groupMap["General"]
				if group == nil {
					ppd.Groups = append(ppd.Groups, PPDGroup{Name: "General", Text: "General"})
					groupMap["General"] = &ppd.Groups[len(ppd.Groups)-1]
					group = groupMap["General"]
				}
				opt.Group = group.Name
				group.Options = append(group.Options, opt)
			}
			ppd.OptionDetails[key] = opt
			continue
		}
		if strings.HasPrefix(line, "*PageSize ") {
			if size, ok := parsePPDPageSize(line); ok && size.Name != "" && size.Width > 0 && size.Length > 0 {
				ppd.PageSizes[size.Name] = size
			}
			continue
		}
		if strings.HasPrefix(line, "*ImageableArea ") {
			if name, l, b, r, t, ok := parsePPDImageableArea(line); ok {
				if size, exists := ppd.PageSizes[name]; exists {
					size.Left = l
					size.Bottom = b
					size.Right = r
					size.Top = t
					ppd.PageSizes[name] = size
				} else {
					ppd.PageSizes[name] = PPDPageSize{
						Name:   name,
						Left:   l,
						Bottom: b,
						Right:  r,
						Top:    t,
					}
				}
			}
			continue
		}
		if strings.HasPrefix(line, "*Default") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				key := strings.TrimPrefix(parts[0], "*Default")
				val := strings.TrimSpace(parts[1])
				def := strings.Trim(val, "\"")
				ppd.Defaults[key] = def
				if opt, ok := ppd.OptionDetails[key]; ok {
					opt.Default = def
				}
			}
		}
		if strings.HasPrefix(line, "*ParamCustom") {
			if param, ok := parsePPDCustomParam(line); ok {
				if opt, ok := ppd.OptionDetails[param.Option]; ok {
					opt.Custom = true
					opt.CustomParams = append(opt.CustomParams, param.Param)
					if strings.EqualFold(param.Option, "PageSize") || strings.EqualFold(param.Option, "CustomPageSize") {
						updateCustomSizeBounds(ppd, param.Param)
					}
				}
			}
			continue
		}
		if strings.HasPrefix(line, "*Custom") {
			if name := strings.TrimSpace(strings.TrimPrefix(line, "*Custom")); name != "" {
				key := strings.Fields(name)
				if len(key) > 0 {
					if opt, ok := ppd.OptionDetails[key[0]]; ok {
						opt.Custom = true
					}
				}
			}
		}
		if strings.HasPrefix(line, "*CloseUI:") {
			continue
		}
		if strings.HasPrefix(line, "*") {
			key, choice, text, ok := parsePPDChoiceLine(line)
			if ok {
				ppd.Options[key] = appendIfMissing(ppd.Options[key], choice)
				if opt, ok := ppd.OptionDetails[key]; ok {
					opt.Choices = append(opt.Choices, PPDChoice{Choice: choice, Text: text})
				}
				if key == "Resolution" {
					ppd.Resolutions = appendIfMissing(ppd.Resolutions, choice)
				}
				if key == "ColorModel" || key == "ColorMode" || key == "ColorSpace" {
					ppd.ColorSpaces = appendIfMissing(ppd.ColorSpaces, choice)
				}
			} else {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					key := strings.TrimPrefix(parts[0], "*")
					option := strings.Split(parts[1], "/")[0]
					_, hasOpenUI := ppd.Options[key]
					if hasOpenUI || key == "PageSize" || key == "Duplex" || key == "Resolution" || key == "ColorModel" || key == "ColorMode" || key == "ColorSpace" {
						ppd.Options[key] = appendIfMissing(ppd.Options[key], option)
					}
					if key == "Resolution" {
						ppd.Resolutions = appendIfMissing(ppd.Resolutions, option)
					}
					if key == "ColorModel" || key == "ColorMode" || key == "ColorSpace" {
						ppd.ColorSpaces = appendIfMissing(ppd.ColorSpaces, option)
					}
				}
			}
		}
	}
	return ppd, sc.Err()
}

func splitPPDLabel(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	if strings.Contains(value, "/") {
		parts := strings.SplitN(value, "/", 2)
		return strings.TrimSpace(parts[0]), strings.Trim(strings.TrimSpace(parts[1]), "\"")
	}
	return strings.TrimSpace(value), ""
}

func normalizePPDUI(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(value, "boolean"):
		return "boolean"
	case strings.Contains(value, "pickmany"):
		return "pickmany"
	case strings.Contains(value, "pickone"):
		return "pickone"
	default:
		return "pickone"
	}
}

func splitPPDList(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parsePPDChoiceLine(line string) (string, string, string, bool) {
	if !strings.HasPrefix(line, "*") {
		return "", "", "", false
	}
	trimmed := strings.TrimPrefix(line, "*")
	idx := strings.Index(trimmed, ":")
	if idx < 0 {
		return "", "", "", false
	}
	left := strings.TrimSpace(trimmed[:idx])
	parts := strings.Fields(left)
	if len(parts) < 2 {
		return "", "", "", false
	}
	key := parts[0]
	choiceText := strings.Join(parts[1:], " ")
	choiceText = strings.TrimSpace(choiceText)
	choiceText = strings.TrimSuffix(choiceText, ":")
	choice, text := splitPPDLabel(choiceText)
	if choice == "" {
		return "", "", "", false
	}
	if text == "" {
		text = choice
	}
	return key, choice, text, true
}

func parsePPDPrinterPreset(line string) (PPDPrinterPreset, bool) {
	if !strings.HasPrefix(line, "*APPrinterPreset ") {
		return PPDPrinterPreset{}, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "*APPrinterPreset"))
	if rest == "" {
		return PPDPrinterPreset{}, false
	}
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return PPDPrinterPreset{}, false
	}
	left := strings.TrimSpace(parts[0])
	right := strings.TrimSpace(parts[1])
	name, text := splitPPDLabel(left)
	if name == "" {
		return PPDPrinterPreset{}, false
	}
	value := strings.TrimSpace(strings.Trim(right, "\""))
	value = strings.ReplaceAll(value, "\\n", " ")
	value = strings.ReplaceAll(value, "\\r", " ")
	value = strings.ReplaceAll(value, "\\t", " ")
	tokens := strings.Fields(value)
	options := make([]PPDPresetOption, 0, len(tokens)/2)
	for i := 0; i < len(tokens); i++ {
		token := strings.TrimSpace(tokens[i])
		if token == "" || !strings.HasPrefix(token, "*") {
			continue
		}
		if i+1 >= len(tokens) {
			break
		}
		val := strings.Trim(tokens[i+1], "\"")
		options = append(options, PPDPresetOption{
			Option: token,
			Value:  val,
		})
		i++
	}
	return PPDPrinterPreset{
		Name:    name,
		Text:    text,
		Options: options,
	}, true
}

type ppdCustomParamParsed struct {
	Option string
	Param  PPDCustomParam
}

func parsePPDCustomParam(line string) (ppdCustomParamParsed, bool) {
	if !strings.HasPrefix(line, "*ParamCustom") {
		return ppdCustomParamParsed{}, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "*ParamCustom"))
	if rest == "" {
		return ppdCustomParamParsed{}, false
	}
	parts := strings.SplitN(rest, ":", 2)
	left := strings.TrimSpace(parts[0])
	right := ""
	if len(parts) == 2 {
		right = strings.TrimSpace(parts[1])
	}
	leftParts := strings.Fields(left)
	if len(leftParts) < 2 {
		return ppdCustomParamParsed{}, false
	}
	option := strings.TrimSpace(leftParts[0])
	paramName, paramText := splitPPDLabel(strings.Join(leftParts[1:], " "))
	if paramName == "" {
		return ppdCustomParamParsed{}, false
	}
	paramType := "text"
	order := 0
	minVal := 0.0
	maxVal := 0.0
	hasRange := false
	if right != "" {
		tokens := strings.Fields(right)
		if len(tokens) > 0 {
			// CUPS PPD: "order type min max"
			if len(tokens) >= 2 {
				if n, err := strconv.Atoi(tokens[0]); err == nil {
					order = n
					paramType = normalizePPDCustomType(tokens[1])
					if len(tokens) >= 4 {
						if minParsed, err := strconv.ParseFloat(tokens[2], 64); err == nil {
							if maxParsed, err2 := strconv.ParseFloat(tokens[3], 64); err2 == nil {
								minVal = minParsed
								maxVal = maxParsed
								hasRange = true
							}
						}
					}
				} else {
					paramType = normalizePPDCustomType(tokens[0])
					if len(tokens) >= 3 {
						if minParsed, err := strconv.ParseFloat(tokens[1], 64); err == nil {
							if maxParsed, err2 := strconv.ParseFloat(tokens[2], 64); err2 == nil {
								minVal = minParsed
								maxVal = maxParsed
								hasRange = true
							}
						}
					}
				}
			} else {
				paramType = normalizePPDCustomType(tokens[0])
			}
		}
	}
	if strings.EqualFold(paramName, "Units") {
		paramType = "units"
	}
	return ppdCustomParamParsed{
		Option: option,
		Param: PPDCustomParam{
			Name:  paramName,
			Text:  paramText,
			Type:  paramType,
			Order: order,
			Min:   minVal,
			Max:   maxVal,
			Range: hasRange,
		},
	}, true
}

func normalizePPDCustomType(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "integer":
		return "int"
	case "float", "double":
		return "real"
	case "passcode":
		return "password"
	default:
		return v
	}
}

func parsePPDFilterLine(line string, isFilter2 bool) (PPDFilter, bool) {
	if isFilter2 {
		line = strings.TrimSpace(strings.TrimPrefix(line, "*cupsFilter2:"))
	} else {
		line = strings.TrimSpace(strings.TrimPrefix(line, "*cupsFilter:"))
	}
	line = strings.TrimSpace(strings.Trim(line, "\""))
	if line == "" {
		return PPDFilter{}, false
	}
	parts := strings.Fields(line)
	if isFilter2 {
		if len(parts) < 4 {
			return PPDFilter{}, false
		}
		cost := 0
		fmt.Sscanf(parts[2], "%d", &cost)
		prog := strings.Join(parts[3:], " ")
		return PPDFilter{
			Source:  parts[0],
			Dest:    parts[1],
			Cost:    cost,
			Program: prog,
		}, prog != ""
	}
	if len(parts) < 3 {
		return PPDFilter{}, false
	}
	cost := 0
	fmt.Sscanf(parts[1], "%d", &cost)
	prog := strings.Join(parts[2:], " ")
	return PPDFilter{
		Source:  parts[0],
		Dest:    "",
		Cost:    cost,
		Program: prog,
	}, prog != ""
}

func parsePPDConstraint(line string) (PPDConstraint, bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return PPDConstraint{}, false
	}
	raw := strings.TrimSpace(line[idx+1:])
	if raw == "" {
		return PPDConstraint{}, false
	}
	fields := strings.Fields(raw)
	if len(fields) < 2 {
		return PPDConstraint{}, false
	}
	opt1 := strings.TrimPrefix(fields[0], "*")
	switch len(fields) {
	case 2:
		opt2 := strings.TrimPrefix(fields[1], "*")
		return PPDConstraint{Option1: opt1, Option2: opt2}, true
	case 3:
		if strings.HasPrefix(fields[1], "*") {
			opt2 := strings.TrimPrefix(fields[1], "*")
			return PPDConstraint{Option1: opt1, Option2: opt2, Choice2: fields[2]}, true
		}
		opt2 := strings.TrimPrefix(fields[2], "*")
		return PPDConstraint{Option1: opt1, Choice1: fields[1], Option2: opt2}, true
	default:
		opt2 := strings.TrimPrefix(fields[2], "*")
		return PPDConstraint{Option1: opt1, Choice1: fields[1], Option2: opt2, Choice2: fields[3]}, true
	}
}

func parsePPDOrderDependency(line string) (PPDOrderDependency, bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return PPDOrderDependency{}, false
	}
	raw := strings.TrimSpace(line[idx+1:])
	if raw == "" {
		return PPDOrderDependency{}, false
	}
	fields := strings.Fields(raw)
	if len(fields) < 3 {
		return PPDOrderDependency{}, false
	}
	order := 0
	if n, err := strconv.Atoi(fields[0]); err == nil {
		order = n
	}
	section := fields[1]
	option := strings.TrimPrefix(fields[2], "*")
	return PPDOrderDependency{Order: order, Section: section, Option: option}, true
}

var pageSizeRe = regexp.MustCompile(`(?i)pagesize\s*\[\s*([0-9.]+)\s+([0-9.]+)\s*\]`)
var imageableAreaRe = regexp.MustCompile(`(?i)^\\*ImageableArea\\s+([^:]+):\\s*\"?\\s*([0-9.]+)\\s+([0-9.]+)\\s+([0-9.]+)\\s+([0-9.]+)\\s*\"?`)
var hwMarginsRe = regexp.MustCompile(`(?i)^\\*HWMargins:\\s*\"?\\s*([0-9.]+)\\s+([0-9.]+)\\s+([0-9.]+)\\s+([0-9.]+)\\s*\"?`)

func parsePPDPageSize(line string) (PPDPageSize, bool) {
	if !strings.HasPrefix(line, "*PageSize ") {
		return PPDPageSize{}, false
	}
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return PPDPageSize{}, false
	}
	left := strings.TrimSpace(strings.TrimPrefix(parts[0], "*PageSize"))
	if left == "" {
		return PPDPageSize{}, false
	}
	left = strings.TrimSpace(strings.SplitN(left, "/", 2)[0])
	if left == "" {
		return PPDPageSize{}, false
	}
	raw := parts[1]
	raw = strings.Trim(raw, " \"")
	matches := pageSizeRe.FindStringSubmatch(raw)
	if len(matches) < 3 {
		return PPDPageSize{}, false
	}
	w, err1 := strconv.ParseFloat(matches[1], 64)
	h, err2 := strconv.ParseFloat(matches[2], 64)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return PPDPageSize{}, false
	}
	return PPDPageSize{
		Name:   left,
		Width:  pointsToPWG(w),
		Length: pointsToPWG(h),
	}, true
}

func parsePPDImageableArea(line string) (string, int, int, int, int, bool) {
	matches := imageableAreaRe.FindStringSubmatch(line)
	if len(matches) < 6 {
		return "", 0, 0, 0, 0, false
	}
	name := strings.TrimSpace(matches[1])
	if name == "" {
		return "", 0, 0, 0, 0, false
	}
	left, err1 := strconv.ParseFloat(matches[2], 64)
	bottom, err2 := strconv.ParseFloat(matches[3], 64)
	right, err3 := strconv.ParseFloat(matches[4], 64)
	top, err4 := strconv.ParseFloat(matches[5], 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return "", 0, 0, 0, 0, false
	}
	return name, pointsToPWG(left), pointsToPWG(bottom), pointsToPWG(right), pointsToPWG(top), true
}

func parsePPDHWMargins(line string) ([4]int, bool) {
	var margins [4]int
	matches := hwMarginsRe.FindStringSubmatch(line)
	if len(matches) < 5 {
		return margins, false
	}
	left, err1 := strconv.ParseFloat(matches[1], 64)
	bottom, err2 := strconv.ParseFloat(matches[2], 64)
	right, err3 := strconv.ParseFloat(matches[3], 64)
	top, err4 := strconv.ParseFloat(matches[4], 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return margins, false
	}
	margins[0] = pointsToPWG(left)
	margins[1] = pointsToPWG(bottom)
	margins[2] = pointsToPWG(right)
	margins[3] = pointsToPWG(top)
	return margins, true
}

func updateCustomSizeBounds(ppd *PPD, param PPDCustomParam) {
	if ppd == nil {
		return
	}
	name := strings.ToLower(strings.TrimSpace(param.Name))
	if name == "" {
		return
	}
	if name != "width" && name != "height" {
		return
	}
	if !param.Range {
		return
	}
	min := pointsToPWG(param.Min)
	max := pointsToPWG(param.Max)
	if max > 0 {
		if name == "width" {
			if ppd.CustomMaxSize[0] == 0 || max > ppd.CustomMaxSize[0] {
				ppd.CustomMaxSize[0] = max
			}
		} else {
			if ppd.CustomMaxSize[1] == 0 || max > ppd.CustomMaxSize[1] {
				ppd.CustomMaxSize[1] = max
			}
		}
	}
	if min > 0 {
		if name == "width" {
			if ppd.CustomMinSize[0] == 0 || min < ppd.CustomMinSize[0] {
				ppd.CustomMinSize[0] = min
			}
		} else {
			if ppd.CustomMinSize[1] == 0 || min < ppd.CustomMinSize[1] {
				ppd.CustomMinSize[1] = min
			}
		}
	}
}

func pointsToPWG(points float64) int {
	return int(math.Round(points * 2540.0 / 72.0))
}

func appendIfMissing(list []string, val string) []string {
	for _, v := range list {
		if v == val {
			return list
		}
	}
	return append(list, val)
}
