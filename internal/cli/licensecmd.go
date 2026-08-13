// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"

	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/spf13/cobra"
)

// repositoryURL is the public source repository — the single place the full
// license text, disclaimers, and third-party notices live. This carries
// pointers, never the bodies (see licenseView), so the notice stays short and
// cannot drift from the repo. The derived URLs are built from it so they can
// never point at a different repo than one another.
const (
	repositoryURL = "https://github.com/korbit-official/korbit-cli"
	licenseURL    = repositoryURL + "/blob/master/LICENSE"
	disclaimerURL = repositoryURL + "/blob/master/DISCLAIMER.md"
	thirdPartyURL = repositoryURL + "/blob/master/THIRD_PARTY_LICENSES.txt"
)

// licenseView is the `license` result: the copyright, the open-source license,
// and where to read the full terms. It deliberately points at those documents
// rather than reproducing them, so both --json consumers and human readers get
// the same short, stable notice.
type licenseView struct {
	Copyright    string `json:"copyright"`
	License      string `json:"license"`
	SpdxId       string `json:"spdxId"`
	LicenseUrl   string `json:"licenseUrl"`
	Disclaimer   string `json:"disclaimer"`
	OpenApiTerms string `json:"openApiTerms"`
	// Sandbox notes that the bundled Korbit API Sandbox is a separate component
	// under its own terms (not this license) and how to read them.
	Sandbox string `json:"sandbox"`
	// ThirdPartyNotices points at the notices for the open-source modules linked
	// into the binary. They ship in each release archive and are read in the repo.
	ThirdPartyNotices string `json:"thirdPartyNotices"`
}

func (v licenseView) FormatText(w io.Writer) {
	fmt.Fprintf(w, "%s\nLicensed under the %s (SPDX: %s).\n\n", v.Copyright, v.License, v.SpdxId)
	fmt.Fprintf(w, "Full license text:            %s\nDisclaimer (read before use): %s\nThird-party notices:          %s\nKorbit Open API terms of use: %s\n\n",
		v.LicenseUrl, v.Disclaimer, v.ThirdPartyNotices, v.OpenApiTerms)
	fmt.Fprint(w, v.Sandbox)
}

// runLicense implements the `license` command: print the copyright, the
// Apache-2.0 license, and pointers to the full license/disclaimers, the
// third-party notices, and the Korbit Open API terms. It makes no API call and
// reads no state.
func (rt *runtime) runLicense(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return output.Usagef("unexpected argument %q", args[0])
	}
	return rt.Emit("license", licenseView{
		Copyright:         "Copyright (c) 2026 Digital X Co., Ltd.",
		License:           "Apache License, Version 2.0",
		SpdxId:            "Apache-2.0",
		LicenseUrl:        licenseURL,
		Disclaimer:        disclaimerURL,
		OpenApiTerms:      probe.PortalURL,
		ThirdPartyNotices: thirdPartyURL,
		Sandbox:           fmt.Sprintf("The Korbit API Sandbox is a separate component under its own terms, not this license — run `%s sandbox license` to read them.", progname.Name()),
	})
}
