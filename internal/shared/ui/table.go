package ui

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Table represents a simple table for CLI output
type Table struct {
	headers []string
	rows    [][]string
	title   string
}

// NewTable creates a new table
func NewTable(headers []string) *Table {
	return &Table{
		headers: headers,
		rows:    [][]string{},
	}
}

// WithTitle sets the table title
func (t *Table) WithTitle(title string) *Table {
	t.title = title
	return t
}

// AddRow adds a row to the table
func (t *Table) AddRow(row []string) *Table {
	t.rows = append(t.rows, row)
	return t
}

// Render renders the table (Vercel-style)
func (t *Table) Render() string {
	if len(t.rows) == 0 {
		return ""
	}

	// Calculate column widths
	colWidths := make([]int, len(t.headers))
	for i, header := range t.headers {
		colWidths[i] = lipgloss.Width(header)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if i < len(colWidths) {
				width := lipgloss.Width(cell)
				if width > colWidths[i] {
					colWidths[i] = width
				}
			}
		}
	}

	var output strings.Builder

	// Title
	if t.title != "" {
		output.WriteString("\n")
		output.WriteString(titleStyle.Render(t.title))
		output.WriteString("\n\n")
	}

	// Header
	headerParts := make([]string, len(t.headers))
	for i, header := range t.headers {
		styled := tableHeaderStyle.Render(header)
		headerParts[i] = padRight(styled, colWidths[i])
	}
	output.WriteString(strings.Join(headerParts, "  "))
	output.WriteString("\n")

	// Separator line
	separatorChar := "─"
	if runtime.GOOS == "windows" {
		separatorChar = "-"
	}
	separatorParts := make([]string, len(t.headers))
	for i := range t.headers {
		separatorParts[i] = mutedStyle.Render(strings.Repeat(separatorChar, colWidths[i]))
	}
	output.WriteString(strings.Join(separatorParts, "  "))
	output.WriteString("\n")

	// Rows
	for _, row := range t.rows {
		rowParts := make([]string, len(t.headers))
		for i, cell := range row {
			if i < len(colWidths) {
				rowParts[i] = padRight(cell, colWidths[i])
			}
		}
		output.WriteString(strings.Join(rowParts, "  "))
		output.WriteString("\n")
	}

	output.WriteString("\n")
	return output.String()
}

// padRight pads
func padRight(text string, targetWidth int) string {
	visibleWidth := lipgloss.Width(text)
	if visibleWidth >= targetWidth {
		return text
	}
	padding := strings.Repeat(" ", targetWidth-visibleWidth)
	return text + padding
}

// Print prints the table
func (t *Table) Print() {
	fmt.Print(t.Render())
}

// RenderList renders a simple list with bullet points
func RenderList(items []string) string {
	bullet := "•"
	if runtime.GOOS == "windows" {
		bullet = "*"
	}
	var output strings.Builder
	for _, item := range items {
		output.WriteString(mutedStyle.Render("  " + bullet + " "))
		output.WriteString(item)
		output.WriteString("\n")
	}
	return output.String()
}

// RenderNumberedList renders a numbered list
func RenderNumberedList(items []string) string {
	var output strings.Builder
	for i, item := range items {
		output.WriteString(mutedStyle.Render(fmt.Sprintf("  %d. ", i+1)))
		output.WriteString(item)
		output.WriteString("\n")
	}
	return output.String()
}
