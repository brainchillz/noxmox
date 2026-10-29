# Minimal stand-in for PVE::Tools so `perl -c pve/NoxPlugin.pm` runs on a
# machine without Proxmox. Only the symbols the plugin names at compile time.
package PVE::Tools;
use strict;
use warnings;
sub run_with_timeout { die "stub\n" }
sub run_command      { die "stub\n" }
1;
