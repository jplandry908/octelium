// Copyright Octelium Labs, LLC. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package authenticator

import (
	"github.com/octelium/octelium/apis/main/metav1"
	"github.com/octelium/octelium/client/common/cliutils"
	"github.com/spf13/cobra"
)

var Cmd = &cobra.Command{
	Use:   "authenticator",
	Short: "Authenticate with an Authenticator",
	Example: `
 octelium auth delete authn totp-123456
 octelium auth delete authenticator totp-abcdef
		 `,
	Aliases: []string{"authn"},
	RunE: func(cmd *cobra.Command, args []string) error {
		return doCmd(cmd, args)
	},
}

func doCmd(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	i, err := cliutils.GetCLIInfo(cmd, args)
	if err != nil {
		return err
	}

	c, err := cliutils.NewAuthClient(ctx, i.Domain, nil)
	if err != nil {
		return err
	}

	if _, err := c.C().DeleteAuthenticator(ctx, &metav1.DeleteOptions{
		Name: i.FirstArg(),
	}); err != nil {
		return err
	}

	return nil
}
