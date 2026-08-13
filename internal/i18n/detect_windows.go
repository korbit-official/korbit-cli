// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package i18n

import "golang.org/x/sys/windows"

// osUILanguages returns the user's preferred UI languages as BCP-47 names
// (e.g. "ko-KR", "en-US"), most preferred first, or nil on error. Windows hosts
// rarely set the POSIX locale env, so this is the locale source there.
func osUILanguages() []string {
	langs, err := windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME)
	if err != nil {
		return nil
	}
	return langs
}
