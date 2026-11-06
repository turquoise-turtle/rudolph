#
# DynamoDB table to store rules
#
locals {
  dynamodb_table_name = var.dynamodb_table_name == "" ? "${var.prefix}_rudolph_store" : var.dynamodb_table_name
}

resource "aws_dynamodb_table" "store" {
  name         = local.dynamodb_table_name
  billing_mode = "PAY_PER_REQUEST"

  hash_key  = "PK"
  range_key = "SK"

  attribute {
    name = "PK"
    type = "S"
  }

  attribute {
    name = "SK"
    type = "S"
  }

  attribute {
    name = "DataType"
    type = "S"
  }

  attribute {
    name = "SerialNum"
    type = "S"
  }

  attribute {
    name = "MachineID"
    type = "S"
  }

  attribute {
    name = "HostnameKey"
    type = "S"
  }

  ttl {
    attribute_name = "ExpiresAfter"
    enabled        = true
  }

  global_secondary_index {
    name               = "SerialNum_DataType_MachineID"
    hash_key           = "SerialNum"
    range_key          = "DataType"
    projection_type    = "INCLUDE"
    non_key_attributes = ["MachineID"]
  }

  # Machine ID lookups by prefix (sensordata.MachineID_DataType_GSI). MachineID is the index range key, so KEYS_ONLY
  # already projects it.
  global_secondary_index {
    name            = "DataType_MachineID"
    hash_key        = "DataType"
    range_key       = "MachineID"
    projection_type = "KEYS_ONLY"
  }

  # The web view's machine list, ordered by serial and searched by serial prefix (sensor data rows only). The
  # projection is limited to attributes that rarely change: projecting Time, ExpiresAfter, RulesHash or the rule
  # counts would cost a write to this index on every sync. Each of the two machine list indexes also projects the
  # other's range key, so both return the same row shape (a list ordered by hostname still shows the serial).
  # Rudolph itself does not query these indexes.
  global_secondary_index {
    name               = "DataType_SerialNum"
    hash_key           = "DataType"
    range_key          = "SerialNum"
    projection_type    = "INCLUDE"
    non_key_attributes = ["HostnameKey", "MachineID", "Hostname", "ModelIdentifier", "OSVersion", "SantaVersion", "ClientMode", "PrimaryUser", "LastSeenDay"]
  }

  # The web view's case-insensitive hostname prefix search. HostnameKey is the trimmed, lower-case hostname
  # (sensordata.HostnameKey); same small projection as DataType_SerialNum, for the same reason.
  global_secondary_index {
    name               = "DataType_HostnameKey"
    hash_key           = "DataType"
    range_key          = "HostnameKey"
    projection_type    = "INCLUDE"
    non_key_attributes = ["SerialNum", "MachineID", "Hostname", "ModelIdentifier", "OSVersion", "SantaVersion", "ClientMode", "PrimaryUser", "LastSeenDay"]
  }

  server_side_encryption {
    enabled     = true
    kms_key_arn = aws_kms_key.store_sse_key.arn
  }

  point_in_time_recovery {
    enabled = true
  }
}
