resource "aws_kms_key" "enos_key" {
  description             = "Enos Key"
  deletion_window_in_days = 7
}

resource "aws_kms_alias" "enos_key_alias" {
  name          = "alias/enos_key"
  target_key_id = aws_kms_key.enos_key.key_id
}

# Replace these with base64encode(file(something))
resource "aws_kms_ciphertext" "enos_vault_license" {
  key_id    = aws_kms_key.enos_key.key_id
  plaintext = "this is a fake license"
}

data "aws_kms_secrets" "enos" {
  secret {
    name    = "vault_license"
    payload = aws_kms_ciphertext.enos_vault_license.ciphertext_blob
  }
}
