/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package banner

import (
	"fmt"
	"os"

	isatty "github.com/mattn/go-isatty"
)

// green matches the StreamShare logo's stroke color (#65c86b).
const (
	green = "\x1b[38;2;101;200;107m"
	bold  = "\x1b[1m"
	reset = "\x1b[0m"
)

// art is the ASCII rendering of the StreamShare logo: the three converging
// streams and play signal carved out of a solid mark as negative space.
var art = [23]string{
	`     @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@     `,
	`   @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@   `,
	` @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@ `,
	`@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@`,
	`@@@@@@   @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@`,
	`@@@@@@       @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@`,
	`@@@@@@          @@@@@@@@  @@@@@@@@@@@@@@@@@`,
	`@@@@@@@@@          @@@@@    @@@@@@@@@@@@@@@`,
	`@@@@@@@@@@@@          @@       @@@@@@@@@@@@`,
	`@@@@@@@@@@@@@@@                  @@@@@@@@@@`,
	`@@@@@@                              @@@@@@@`,
	`@@@@@@                                @@@@@`,
	`@@@@@@                              @@@@@@@`,
	`@@@@@@@@@@@@@@@                  @@@@@@@@@@`,
	`@@@@@@@@@@@@          @@       @@@@@@@@@@@@`,
	`@@@@@@@@@          @@@@@    @@@@@@@@@@@@@@@`,
	`@@@@@@          @@@@@@@@  @@@@@@@@@@@@@@@@@`,
	`@@@@@@       @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@`,
	`@@@@@@   @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@`,
	`@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@`,
	` @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@ `,
	`   @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@   `,
	`     @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@     `,
}

// label is centered under the mark.
const label = "stream-share"

// Print writes an ASCII rendering of the StreamShare logo to stdout, colored
// when stdout is a terminal and plain otherwise (e.g. when piped to a log
// collector).
func Print() {
	color := isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())

	for _, line := range art {
		if color {
			fmt.Println(green + line + reset)
		} else {
			fmt.Println(line)
		}
	}

	pad := (len(art[0]) - len(label)) / 2
	padded := fmt.Sprintf("%*s%s", pad, "", label)
	if color {
		fmt.Println(bold + padded + reset)
	} else {
		fmt.Println(padded)
	}
}
