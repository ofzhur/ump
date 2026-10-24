package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/ofzhur/ump/internal/cache"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

type outputFormat string

const (
	formatTable outputFormat = "table"
	formatWide  outputFormat = "wide"
	formatJSON  outputFormat = "json"
	formatYAML  outputFormat = "yaml"
	formatName  outputFormat = "name"
	// formatAnsible is the inventory of ump server list -o ansible alone.
	formatAnsible outputFormat = "ansible"
)

var outputFormats = []string{"table", "wide", "json", "yaml", "name"}

func parseFormat(s string) (outputFormat, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return formatTable, nil
	}
	for _, f := range outputFormats {
		if s == f {
			return outputFormat(s), nil
		}
	}
	return "", fmt.Errorf("unknown output format %q (use one of %s)", s, strings.Join(outputFormats, ", "))
}

// column is one table column of `ump server list`.
type column struct {
	header string
	wide   bool
	value  func(provider.Machine) string
}

var serverColumns = []column{
	{header: "ACCOUNT", value: func(m provider.Machine) string { return m.Account }},
	{header: "PROVIDER", value: func(m provider.Machine) string { return m.Provider }},
	{header: "ID", wide: true, value: func(m provider.Machine) string { return m.ID }},
	{header: "NAME", value: func(m provider.Machine) string { return m.Name }},
	{header: "IPV4", value: func(m provider.Machine) string { return m.IPv4 }},
	{header: "LOCATION", value: func(m provider.Machine) string { return m.Location }},
	{header: "STATUS", value: func(m provider.Machine) string { return m.Status }},
	{header: "OS", value: func(m provider.Machine) string { return m.OS }},
	{header: "GROUPS", value: func(m provider.Machine) string { return strings.Join(m.Groups, ",") }},
	{header: "PROFILE", wide: true, value: func(m provider.Machine) string { return m.Profile }},
	{header: "LAYOUT", wide: true, value: func(m provider.Machine) string { return m.Layout }},
	{header: "IPV6", wide: true, value: func(m provider.Machine) string { return m.IPv6 }},
	{header: "PRIVATE", wide: true, value: func(m provider.Machine) string { return m.PrivateAddress }},
	{header: "PRODUCT", wide: true, value: func(m provider.Machine) string { return m.Product }},
	{header: "KIND", wide: true, value: func(m provider.Machine) string { return string(m.Kind) }},
	{header: "PRICE", value: func(m provider.Machine) string { return m.Price.String() }},
	{header: "ATTRIBUTES", wide: true, value: func(m provider.Machine) string { return joinMap(m.Attributes) }},
}

// joinMap renders a map as k=v pairs, sorted.
func joinMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

// printMachines writes machines in the requested format. JSON output is a
// plain array, so that `ump server list -o json | jq '.[]'` works.
func printMachines(w io.Writer, ms []provider.Machine, f outputFormat, noHeaders bool) error {
	if ms == nil {
		ms = []provider.Machine{}
	}
	switch f {
	case formatJSON, formatYAML:
		return printEncoded(w, ms, f)
	case formatName:
		for _, m := range ms {
			if _, err := fmt.Fprintln(w, m.Ref()); err != nil {
				return err
			}
		}
		return nil
	}
	cols := visibleColumns(f == formatWide)
	rows := make([][]string, 0, len(ms))
	for _, m := range ms {
		rows = append(rows, machineCells(cols, m))
	}
	return writeTable(w, columnHeaders(cols), rows, noHeaders)
}

// printChanges writes a diff: one row per added, removed or changed
// machine, with a CHANGE column first and the changed fields last.
func printChanges(w io.Writer, changes []cache.Change, f outputFormat, noHeaders bool) error {
	if f == formatName {
		marks := map[string]string{cache.Added: "+", cache.Removed: "-", cache.Changed: "~"}
		for _, c := range changes {
			if _, err := fmt.Fprintf(w, "%s %s\n", marks[c.Change], c.Machine.Ref()); err != nil {
				return err
			}
		}
		return nil
	}
	cols := visibleColumns(f == formatWide)
	headers := append(append([]string{"CHANGE"}, columnHeaders(cols)...), "DETAILS")
	rows := make([][]string, 0, len(changes))
	for _, c := range changes {
		row := append(append([]string{c.Change}, machineCells(cols, c.Machine)...), changeDetails(c))
		rows = append(rows, row)
	}
	return writeTable(w, headers, rows, noHeaders)
}

// changeDetails renders "field: before → after" for a changed machine.
func changeDetails(c cache.Change) string {
	if c.Change != cache.Changed || c.Before == nil {
		return ""
	}
	parts := make([]string, 0, len(c.Fields))
	for _, f := range c.Fields {
		before, after := cache.FieldValue(*c.Before, f), cache.FieldValue(c.Machine, f)
		if before == "" {
			before = "<none>"
		}
		if after == "" {
			after = "<none>"
		}
		parts = append(parts, f+": "+before+" → "+after)
	}
	return strings.Join(parts, "; ")
}

// printEncoded writes v as JSON or YAML.
func printEncoded(w io.Writer, v any, f outputFormat) error {
	if f == formatJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	data, err := config.MarshalYAML(v)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func visibleColumns(wide bool) []column {
	var cols []column
	for _, c := range serverColumns {
		if wide || !c.wide {
			cols = append(cols, c)
		}
	}
	return cols
}

func columnHeaders(cols []column) []string {
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = c.header
	}
	return headers
}

func machineCells(cols []column, m provider.Machine) []string {
	cells := make([]string, len(cols))
	for i, c := range cols {
		v := c.value(m)
		if v == "" {
			v = "<none>"
		}
		cells[i] = v
	}
	return cells
}

// writeTable aligns rows kubectl style (three spaces between columns)
// without trailing whitespace.
func writeTable(w io.Writer, headers []string, rows [][]string, noHeaders bool) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 8, 3, ' ', 0)
	if !noHeaders {
		fmt.Fprintln(tw, strings.Join(headers, "\t"))
	}
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, line := range strings.SplitAfter(buf.String(), "\n") {
		if line == "" {
			continue
		}
		if _, err := io.WriteString(w, strings.TrimRight(line, " \n")+"\n"); err != nil {
			return err
		}
	}
	return nil
}
