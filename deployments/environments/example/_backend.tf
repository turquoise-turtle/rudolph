terraform {
  backend "s3" {
    # The S3 bucket in which the Terraform backend state is kept.
    #
    # This S3 bucket isn't created during a terraform apply and needs to be pre-existing.
    bucket = "your-terraform-tfstate-bucket"

    # The S3 key should be unique within the tfstate bucket
    key = "rudolph-example-prefix.tfstate"

    region  = "us-east-1"
    acl     = "private"
    encrypt = true

    # The KMS key Id used for SSE on the terraform S3 bucket, where applicable.
    kms_key_id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

    # Locks the state with a <key>.tflock object next to it, so two applies at once cannot both write the state. The
    # state bucket's policy must allow s3:GetObject, s3:PutObject and s3:DeleteObject on that object too.
    use_lockfile = true
  }
}

provider "aws" {
  region = "us-east-1"
  default_tags {
    tags = {
      Name = "Rudolph"
    }
  }
}
