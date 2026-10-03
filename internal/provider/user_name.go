// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// ECMAScript \\s includes Unicode whitespace that Go regexp \\s omits.
var outlineUserNameURL = regexp.MustCompile(`(www\.|file:|http:|https:)[^\t\n\v\f\r \x{00a0}\x{feff}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]+[\w]`)

// The User ORM validators differ from group Zod validation: Length counts
// Unicode code points, and NotContainsUrl rejects the release's URL pattern.
type outlineUserName struct{}

func (outlineUserName) Description(context.Context) string {
	return "must contain 1 to 255 Unicode code points, a non-whitespace character, and no URL"
}
func (v outlineUserName) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (v outlineUserName) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	name := req.ConfigValue.ValueString()
	if n := utf8.RuneCountInString(name); n < 1 || n > 255 || strings.TrimSpace(name) == "" || outlineUserNameURL.MatchString(name) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid user name", v.Description(ctx))
	}
}
