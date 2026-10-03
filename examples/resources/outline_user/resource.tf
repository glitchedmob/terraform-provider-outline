resource "outline_user" "alice" {
  email              = "alice@example.com"
  role               = "member"
  suspended          = false
  suppress_email     = true
  delete_permanently = false

  # Leave name omitted to let the IdP supply it on sign-in.
}
