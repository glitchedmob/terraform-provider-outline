// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// Outline's Zod string limits use JavaScript length, which counts UTF-16 code
// units, not UTF-8 bytes or Unicode code points. Supplementary runes count twice.
type outlineStringLength struct{ min, max int }

func (v outlineStringLength) Description(context.Context) string {
	return fmt.Sprintf("must contain between %d and %d UTF-16 code units", v.min, v.max)
}

func (v outlineStringLength) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v outlineStringLength) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	length := 0
	for _, r := range req.ConfigValue.ValueString() {
		length++
		if r > 0xffff {
			length++
		}
	}
	if length < v.min || length > v.max {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid string length", v.Description(ctx))
	}
}
