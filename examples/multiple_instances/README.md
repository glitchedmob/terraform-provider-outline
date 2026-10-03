# Two Outline instances

This example uses `outline.eu` and `outline.us` with separate API URLs and required sensitive API-key variables. It manages all six resources and reads all three data sources in each instance. Every resource and lookup selects its alias explicitly; every parent reference stays in that same instance.

Replace both example URLs and emails before applying. Supply each API key outside committed files. For Bash, prompts avoid putting keys into shell history:

```shell
export TF_VAR_eu_base_url="https://outline-eu.example.com/api"
export TF_VAR_us_base_url="https://outline-us.example.com/api"
read -r -s -p "EU Outline admin API key: " TF_VAR_eu_api_key
printf '\n'
export TF_VAR_eu_api_key
read -r -s -p "US Outline admin API key: " TF_VAR_us_api_key
printf '\n'
export TF_VAR_us_api_key
```

Use active admin-owned keys from the corresponding workspaces. Neither key may belong to the account whose user resource or direct collection grant it manages. The provider does not create API keys. The sensitive flag hides values in normal CLI output; it does not encrypt files, plans, state, or backups. Protect those artifacts.

The intended Registry source address is not published yet; use a local development build. Documentation validation does not execute these examples.

Import uses the alias configured on the target resource block. For example, an import into `outline_collection_user.eu` reads through `outline.eu`. Match both EU parent UUIDs and the existing required permission before importing. Do not reuse UUIDs from the other instance. Import existing accounts and grants rather than trying to create them again. Keep both provider configurations while their resources remain in state, including for destruction.

Direct and group grants are separate from default collection permission and effective access. The creator's automatic direct admin grant is not managed here. To manage that pair intentionally, use another active admin's key and import it; a new grant resource never silently adopts it. The collection destruction guards are false. Apply an intentional guard change before deleting content, or remove state and configuration to stop managing without deletion.

This example targets Outline 1.10.1 and does not test an OIDC handshake, IdP login, or identity linking. See the [multiple-instance guide](../../docs/guides/multiple-instances.md) and [integrated IAM example](../iam).
