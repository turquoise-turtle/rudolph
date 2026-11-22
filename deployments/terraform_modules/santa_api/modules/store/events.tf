#
# DynamoDB table of execution and file access events, read by the separate web view (rudolph-web's docs/data-layer-design.md, "D+ in
# detail"). The eventupload Lambda writes it; the scheduled ranker (../../ranker.tf) reads its day counters and machine
# markers and writes the top lists. Layout and costs: docs/events-table.md.
#
locals {
  events_table_name = var.events_dynamodb_table_name == "" ? "${var.prefix}_rudolph_events" : var.events_dynamodb_table_name
}

resource "aws_dynamodb_table" "events" {
  name         = local.events_table_name
  billing_mode = "PAY_PER_REQUEST"

  # Execution items: PK Exec#<machine ID>, SK <20-digit µs execution time>#<file SHA-256>#<pid>. File access items:
  # PK FileAccess#<machine ID>, SK <20-digit µs access time>#<pid>#<16 hex digits>; they carry no TeamID, so they
  # stay out of TeamID_TS. Day counters (BinaryDay#<UTC date> / <file SHA-256>) and machine markers
  # (BinarySeen#<file SHA-256> / <machine ID>) share the table; they carry no TS, so they stay out of every index.
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
    name = "Fleet"
    type = "S"
  }

  attribute {
    name = "Decision"
    type = "S"
  }

  attribute {
    name = "TeamID"
    type = "S"
  }

  attribute {
    name = "TS"
    type = "S"
  }

  ttl {
    attribute_name = "ExpiresAfter"
    enabled        = true
  }

  # Every event of the fleet, newest first. Fleet is Exec on every execution and FileAccess on every file access
  # event, so this index has two partitions; at the event rates in docs/events-table.md that is far below a
  # partition's write limit.
  global_secondary_index {
    name = "Fleet_TS"
    key_schema {
      attribute_name = "Fleet"
      key_type       = "HASH"
    }
    key_schema {
      attribute_name = "TS"
      key_type       = "RANGE"
    }
    projection_type = "ALL"
  }

  global_secondary_index {
    name = "Decision_TS"
    key_schema {
      attribute_name = "Decision"
      key_type       = "HASH"
    }
    key_schema {
      attribute_name = "TS"
      key_type       = "RANGE"
    }
    projection_type = "ALL"
  }

  # Sparse: unsigned binaries have no team ID, and the writer leaves the attribute out. Executions only.
  global_secondary_index {
    name = "TeamID_TS"
    key_schema {
      attribute_name = "TeamID"
      key_type       = "HASH"
    }
    key_schema {
      attribute_name = "TS"
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

#
# Writes from the eventupload Lambda: items of its own partitions of this table only, plus reading the index of
# flagged rules and the rule rows it names in the store table, and taking stale entries out of that index. Kept apart
# from lambda_permissions, which is scoped to the store table and its machine partitions.
# TransactWriteItems (an event with its day counter) has no IAM action of its own: it needs PutItem and UpdateItem on
# the table. Every ForAllValues condition on dynamodb:LeadingKeys has a Null check beside it: ForAllValues is also
# true when the request names no partition at all.
#
data "aws_iam_policy_document" "events_write" {
  # Execution events (Exec#, in a transaction with their day counter's UpdateItem), file access events (FileAccess#),
  # day counters (BinaryDay#), machine markers (BinarySeen#) and each Mac's unknown markers (UnknownSeen#, read by the
  # fleet job). The ranker's TopSnapshot# and Top# rows and the fleet job's FleetSnapshot# and FleetPointer rows are
  # not eventupload's to write.
  statement {
    actions = [
      "dynamodb:PutItem",
      "dynamodb:UpdateItem",
    ]

    resources = [
      aws_dynamodb_table.events.arn,
    ]

    condition {
      test     = "ForAllValues:StringLike"
      variable = "dynamodb:LeadingKeys"
      values   = ["Exec#*", "FileAccess#*", "BinaryDay#*", "BinarySeen#*", "UnknownSeen#*"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }

  # Which rules have their allowed executions stored is read from one item of the store table (PK "EventCollection",
  # see pkg/model/eventcollection), and confirmed on the rule rows it names before anything is stored. A flagged group
  # rule also needs the uploading Mac's groups: its Groups item (the Mac's own partition, "Machine#<id>"; IAM cannot
  # narrow a grant to one sort key, so the Mac's other items are readable too) and the group registry, then the rule
  # rows of the Mac's groups.
  statement {
    actions = [
      "dynamodb:GetItem",
    ]

    resources = [
      aws_dynamodb_table.store.arn,
    ]

    condition {
      test     = "ForAllValues:StringLike"
      variable = "dynamodb:LeadingKeys"
      values   = ["EventCollection", "GlobalRules", "GroupRegistry", "GroupRules#*", "Machine#*", "MachineRules#*"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }

  # A rule that no longer collects is taken out of that item, in a transaction that checks its row has not been
  # flagged again in the meantime. Nothing else of the store table can be written.
  statement {
    actions = [
      "dynamodb:UpdateItem",
    ]

    resources = [
      aws_dynamodb_table.store.arn,
    ]

    condition {
      test     = "ForAllValues:StringEquals"
      variable = "dynamodb:LeadingKeys"
      values   = ["EventCollection"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }

  statement {
    actions = [
      "dynamodb:ConditionCheckItem",
    ]

    resources = [
      aws_dynamodb_table.store.arn,
    ]

    condition {
      test     = "ForAllValues:StringLike"
      variable = "dynamodb:LeadingKeys"
      values   = ["GlobalRules", "GroupRules#*", "MachineRules#*"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }
}

resource "aws_iam_policy" "events_write" {
  name   = "${var.prefix}_rudolph_events_write_policy"
  policy = data.aws_iam_policy_document.events_write.json
}

resource "aws_iam_role_policy_attachment" "events_write" {
  count      = length(var.events_write_lambda_role_names)
  role       = element(var.events_write_lambda_role_names, count.index)
  policy_arn = aws_iam_policy.events_write.arn
}
