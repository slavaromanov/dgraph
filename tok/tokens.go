/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package tok

import (
	"github.com/slavaromanov/dgraph/x"
)

//  Might want to allow user to replace this.
var termTokenizer TermTokenizer
var fullTextTokenizer FullTextTokenizer

func GetTokens(funcArgs []string) ([]string, error) {
	return tokenize(funcArgs, termTokenizer)
}

func GetTextTokens(funcArgs []string, lang string) ([]string, error) {
	t, found := GetTokenizer("fulltext" + lang)
	if found {
		return tokenize(funcArgs, t)
	}
	return nil, x.Errorf("Tokenizer not found for %s", "fulltext"+lang)
}

func tokenize(funcArgs []string, tokenizer Tokenizer) ([]string, error) {
	if len(funcArgs) != 1 {
		return nil, x.Errorf("Function requires 1 arguments, but got %d",
			len(funcArgs))
	}
	return BuildTokens(funcArgs[0], tokenizer)
}
