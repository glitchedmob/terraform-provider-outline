terraform {
  required_providers {
    outline = {
      source = "glitchedmob/outline"
    }
  }
}

# Set OUTLINE_API_KEY in the environment.
# Set OUTLINE_BASE_URL for a self-hosted instance, including its /api path.
provider "outline" {
  timeout_seconds = 30
}
