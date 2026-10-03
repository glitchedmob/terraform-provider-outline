terraform {
  required_version = ">= 1.0"
  required_providers {
    outline = {
      source = "glitchedmob/outline"
    }
  }
}

# Explicit credentials and URLs keep these clients independent of OUTLINE_*.
provider "outline" {
  alias           = "eu"
  base_url        = var.eu_base_url
  api_key         = var.eu_api_key
  timeout_seconds = 30
}

provider "outline" {
  alias           = "us"
  base_url        = var.us_base_url
  api_key         = var.us_api_key
  timeout_seconds = 30
}

# Every resource and lookup is scoped to outline.eu.
# Alice must not own this instance's provider API key.
resource "outline_user" "eu" {
  provider           = outline.eu
  email              = "alice@eu.example.com"
  role               = "member"
  suspended          = false
  suppress_email     = true
  delete_permanently = false
}

resource "outline_group" "eu" {
  provider         = outline.eu
  name             = "Engineering"
  description      = "Engineering team in EU"
  disable_mentions = false
}

resource "outline_group_member" "eu" {
  provider   = outline.eu
  group_id   = outline_group.eu.id
  user_id    = outline_user.eu.id
  permission = "member"
}

resource "outline_collection" "eu" {
  provider      = outline.eu
  name          = "Engineering"
  description   = "Engineering documentation in EU"
  permission    = null
  sharing       = false
  allow_destroy = false
}

resource "outline_collection_group" "eu" {
  provider      = outline.eu
  collection_id = outline_collection.eu.id
  group_id      = outline_group.eu.id
  permission    = "read_write"
}

# This direct grant does not cap access through the read_write group grant.
# The collection creator's automatic admin grant is not managed here.
resource "outline_collection_user" "eu" {
  provider      = outline.eu
  collection_id = outline_collection.eu.id
  user_id       = outline_user.eu.id
  permission    = "read"
}

data "outline_user" "eu" {
  provider = outline.eu
  id       = outline_user.eu.id
}

data "outline_group" "eu" {
  provider = outline.eu
  id       = outline_group.eu.id
}

data "outline_collection" "eu" {
  provider = outline.eu
  id       = outline_collection.eu.id
}

# Every resource and lookup is scoped to outline.us.
# Alice must not own this instance's provider API key.
resource "outline_user" "us" {
  provider           = outline.us
  email              = "alice@us.example.com"
  role               = "member"
  suspended          = false
  suppress_email     = true
  delete_permanently = false
}

resource "outline_group" "us" {
  provider         = outline.us
  name             = "Engineering"
  description      = "Engineering team in US"
  disable_mentions = false
}

resource "outline_group_member" "us" {
  provider   = outline.us
  group_id   = outline_group.us.id
  user_id    = outline_user.us.id
  permission = "member"
}

resource "outline_collection" "us" {
  provider      = outline.us
  name          = "Engineering"
  description   = "Engineering documentation in US"
  permission    = null
  sharing       = false
  allow_destroy = false
}

resource "outline_collection_group" "us" {
  provider      = outline.us
  collection_id = outline_collection.us.id
  group_id      = outline_group.us.id
  permission    = "read_write"
}

# This direct grant does not cap access through the read_write group grant.
# The collection creator's automatic admin grant is not managed here.
resource "outline_collection_user" "us" {
  provider      = outline.us
  collection_id = outline_collection.us.id
  user_id       = outline_user.us.id
  permission    = "read"
}

data "outline_user" "us" {
  provider = outline.us
  id       = outline_user.us.id
}

data "outline_group" "us" {
  provider = outline.us
  id       = outline_group.us.id
}

data "outline_collection" "us" {
  provider = outline.us
  id       = outline_collection.us.id
}

output "workspace_ids" {
  value = {
    eu = {
      user       = data.outline_user.eu.id
      group      = data.outline_group.eu.id
      collection = data.outline_collection.eu.id
    }
    us = {
      user       = data.outline_user.us.id
      group      = data.outline_group.us.id
      collection = data.outline_collection.us.id
    }
  }
}
