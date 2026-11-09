output "resource_id" {
  value = aws_api_gateway_resource.resource.id
}

output "integration_ids" {
  value = toset([for item in module.api_methods : item.integration_id])
}

# A renamed or moved route keeps its resource ID (path_part and parent_id are updated in place), so the path is hashed
# too.
output "integration_shas" {
  value = join(",", concat(
    [sha1(jsonencode([aws_api_gateway_resource.resource.parent_id, aws_api_gateway_resource.resource.path_part]))],
    sort([for item in module.api_methods : item.integration_sha]),
  ))
}
