variable "eu_base_url" {
  description = "EU Outline API URL, including /api. Replace this example URL."
  type        = string
  default     = "https://outline-eu.example.com/api"
}

variable "us_base_url" {
  description = "US Outline API URL, including /api. Replace this example URL."
  type        = string
  default     = "https://outline-us.example.com/api"
}

variable "eu_api_key" {
  description = "Unrestricted key owned by an active EU workspace admin, not Alice."
  type        = string
  sensitive   = true
}

variable "us_api_key" {
  description = "Unrestricted key owned by an active US workspace admin, not Alice."
  type        = string
  sensitive   = true
}
