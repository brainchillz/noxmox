# Minimal stand-in for PVE::Storage::Plugin so `perl -c pve/NoxPlugin.pm`
# runs on a machine without Proxmox. The plugin only needs the package to
# exist as a base class; nothing here is called.
package PVE::Storage::Plugin;
use strict;
use warnings;
sub register              { }
sub init                  { }
sub parse_volname         { die "stub\n" }
sub get_next_vm_diskname  { die "stub\n" }
sub preallocation_cmd_opt { die "stub\n" }
1;
