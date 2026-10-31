# One-time import of the machines and the firewall group that were created by hand. They do nothing once the resources are in the state.
import {
  for_each = local.machines
  to       = aws_instance.machine[each.key]
  id       = each.value.instance_id
}

import {
  to = aws_security_group.forge_bench
  id = "sg-00f59c0cfcc98d01d"
}
