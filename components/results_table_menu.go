package components

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/jorgerojas26/lazysql/app"
)

type ResultsTableMenuState struct {
	SelectedOption int
}

type ResultsTableMenu struct {
	*tview.TextView
	state   *ResultsTableMenuState
	focused bool
	// Width is the rendered horizontal size of the menu, used to right-align it
	// on the tab header row.
	Width int
}

var menuItems = []string{
	menuRecords,
	menuColumns,
	menuConstraints,
	menuForeignKeys,
	menuIndexes,
}

func NewResultsTableMenu() *ResultsTableMenu {
	state := &ResultsTableMenuState{
		SelectedOption: 1,
	}

	textView := tview.NewTextView()
	textView.SetDynamicColors(true)
	textView.SetTextAlign(tview.AlignRight)

	menu := &ResultsTableMenu{
		TextView: textView,
		state:    state,
	}

	// Reserve a stable width: every label single-space separated, plus the
	// " (n)" shown on the one active item. Exactly one item is always active,
	// so the rendered width is constant and the right-aligned menu never shifts.
	width := len(" (0)")
	for i, item := range menuItems {
		if i > 0 {
			width++ // single space separator
		}
		width += len(item)
	}
	menu.Width = width

	menu.render()

	return menu
}

// render rebuilds the menu line: only the active option shows its "(n)" number,
// items are separated by a single space, and colors reflect the focus state.
func (menu *ResultsTableMenu) render() {
	parts := make([]string, len(menuItems))

	for i, item := range menuItems {
		label := item
		if i+1 == menu.state.SelectedOption {
			label = fmt.Sprintf("%s (%d)", item, i+1)
		}

		var color tcell.Color
		switch {
		case i+1 == menu.state.SelectedOption:
			// Active view is highlighted in yellow, like the active tab name.
			color = app.Styles.SecondaryTextColor
		case !menu.focused:
			color = app.Styles.InverseTextColor
		default:
			color = app.Styles.PrimaryTextColor
		}

		parts[i] = fmt.Sprintf("[%s]%s", colorTag(color), label)
	}

	menu.SetText(strings.Join(parts, " "))
}

// colorTag converts a color into a tview dynamic-color tag value, mapping the
// default color to "-" (which tview resolves to the theme default).
func colorTag(c tcell.Color) string {
	if hex := c.Hex(); hex >= 0 {
		return fmt.Sprintf("#%06x", hex)
	}
	return "-"
}

// Getters and Setters
func (menu *ResultsTableMenu) GetSelectedOption() int {
	return menu.state.SelectedOption
}

func (menu *ResultsTableMenu) SetSelectedOption(option int) {
	if menu.state.SelectedOption != option {
		menu.state.SelectedOption = option
		menu.render()
	}
}

func (menu *ResultsTableMenu) SetBlur() {
	menu.focused = false
	menu.render()
}

func (menu *ResultsTableMenu) SetFocus() {
	menu.focused = true
	menu.render()
}
