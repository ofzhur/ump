package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

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

// column is one table column of `ump get servers`.
type column struct {
	header string
	wide   bool
	value  func(provider.Machine) string
}

var serverColumns = []column{
	{header: "ACCOUNT", value: func(m provider.Machine) string { return m.Account }},
	{header: "PROVIDER", value: func(m provider.Machine) string { return m.Provider }},
	{header: "ID", value: func(m provider.Machine) string { return m.ID }},
	{header: "NAME", value: func(m provider.Machine) string { return m.Name }},
	{header: "IPV4", value: func(m provider.Machine) string { return m.IPv4 }},
	{header: "LOCATION", value: func(m provider.Machine) string { return m.Location }},
	{header: "STATUS", value: func(m provider.Machine) string { return m.Status }},
	{header: "IPV6", wide: true, value: func(m provider.Machine) string { return m.IPv6 }},
	{header: "PRODUCT", wide: true, value: func(m provider.Machine) string { return m.Product }},
	{header: "KIND", wide: true, value: func(m provider.Machine) string { return string(m.Kind) }},
}

// printMachines writes machines in the requested format. JSON output is a
// plain array, so that `ump get servers -o json | jq '.[]'` works.
func printMachines(w io.Writer, ms []provider.Machine, f outputFormat, noHeaders bool) error {
	if ms == nil {
		ms = []provider.Machine{}
	}
	switch f {
	case formatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(ms)
	case formatYAML:
		data, err := config.MarshalYAML(ms)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	case formatName:
		for _, m := range ms {
			if _, err := fmt.Fprintln(w, m.Ref()); err != nil {
				return err
			}
		}
		return nil
	case formatWide:
		return printTable(w, ms, true, noHeaders)
	default:
		return printTable(w, ms, false, noHeaders)
	}
}

func printTable(w io.Writer, ms []provider.Machine, wide, noHeaders bool) error {
	var cols []column
	for _, c := range serverColumns {
		if wide || !c.wide {
			cols = append(cols, c)
		}
	}
	tw := tabwriter.NewWriter(w, 0, 8, 3, ' ', 0)
	if !noHeaders {
		headers := make([]string, len(cols))
		for i, c := range cols {
			headers[i] = c.header
		}
		fmt.Fprintln(tw, strings.Join(headers, "\t"))
	}
	for _, m := range ms {
		cells := make([]string, len(cols))
		for i, c := range cols {
			v := c.value(m)
			if v == "" {
				v = "<none>"
			}
			cells[i] = v
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}
