package server

import "html"

func htmlEscape(s string) string {
	return html.EscapeString(s)
}
