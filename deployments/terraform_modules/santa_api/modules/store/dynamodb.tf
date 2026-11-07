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
    name = "SerialKey"
    type = "S"
  }

  attribute {
    name = "HostnameKey"
    type = "S"
  }

  attribute {
    name = "GlobalRulePolicy"
    type = "N"
  }

  ttl {
    attribute_name = "ExpiresAfter"
    enabled        = true
  }

  global_secondary_index {
    name = "SerialNum_DataType_MachineID"
    key_schema {
      attribute_name = "SerialNum"
      key_type       = "HASH"
    }
    key_schema {
      attribute_name = "DataType"
      key_type       = "RANGE"
    }
    projection_type    = "INCLUDE"
    non_key_attributes = ["MachineID"]
  }

  # Machine ID lookups by prefix (sensordata.MachineID_DataType_GSI). MachineID is the index range key, so KEYS_ONLY
  # already projects it.
  global_secondary_index {
    name = "DataType_MachineID"
    key_schema {
      attribute_name = "DataType"
      key_type       = "HASH"
    }
    key_schema {
      attribute_name = "MachineID"
      key_type       = "RANGE"
    }
    projection_type = "KEYS_ONLY"
  }

  # The web view's machine list, ordered by serial and searched by serial prefix (sensor data rows only). SerialKey
  # is the trimmed, upper-case serial (sensordata.SerialKey), so a search matches mixed-case virtual machine serials;
  # SerialNum is projected so the list shows the serial as reported. The projection is limited to attributes that
  # rarely change: projecting Time, ExpiresAfter, RulesHash or the rule counts would cost a write to this index on
  # every sync. Each machine list index also projects the other's key (HostnameKey here, SerialKey there), so both
  # return the same row shape. Of Rudolph itself, only `rudolph machines` reads DataType_SerialKey.
  global_secondary_index {
    name = "DataType_SerialKey"
    key_schema {
      attribute_name = "DataType"
      key_type       = "HASH"
    }
    key_schema {
      attribute_name = "SerialKey"
      key_type       = "RANGE"
    }
    projection_type    = "INCLUDE"
    non_key_attributes = ["SerialNum", "HostnameKey", "MachineID", "Hostname", "ModelIdentifier", "OSVersion", "SantaVersion", "ClientMode", "PrimaryUser", "LastSeenDay"]
  }

  # The web view's case-insensitive hostname prefix search. HostnameKey is the trimmed, lower-case hostname
  # (sensordata.HostnameKey); same small projection as DataType_SerialKey, for the same reason.
  global_secondary_index {
    name = "DataType_HostnameKey"
    key_schema {
      attribute_name = "DataType"
      key_type       = "HASH"
    }
    key_schema {
      attribute_name = "HostnameKey"
      key_type       = "RANGE"
    }
    projection_type    = "INCLUDE"
    non_key_attributes = ["SerialNum", "SerialKey", "MachineID", "Hostname", "ModelIdentifier", "OSVersion", "SantaVersion", "ClientMode", "PrimaryUser", "LastSeenDay"]
  }

  # The web view's rules page, filtered by policy. GlobalRulePolicy mirrors Policy on global rule rows only (machine
  # rules and the rules feed carry Policy but not GlobalRulePolicy, so they stay out). The page shows every rule
  # attribute, so the index projects them all; global rules change only when an administrator edits them, never per
  # sync, so the extra index writes cost next to nothing. Rudolph itself does not query this index.
  global_secondary_index {
    name = "GlobalRulePolicy_SK"
    key_schema {
      attribute_name = "GlobalRulePolicy"
      key_type       = "HASH"
    }
    key_schema {
      attribute_name = "SK"
      key_type       = "RANGE"
    }
    projection_type = "ALL"
  }

  server_side_encryption {
    enabled     = true
    kms_key_arn = aws_kms_key.store_sse_key.arn
  }

  point_in_time_recovery {
    enabled = true
  }

  deletion_protection_enabled = var.table_deletion_protection
}
