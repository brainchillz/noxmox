# Minimal stand-in for PVE::Tools so `perl -c pve/NoxPlugin.pm` runs on a
# machine without Proxmox. Only the symbols the plugin names at compile time.
package PVE::Tools;
use strict;
use warnings;
use Exporter 'import';
our @EXPORT_OK = qw(run_command);
sub run_with_timeout { die "stub\n" }
sub run_command      { die "stub\n" }
sub file_get_contents { die "stub\n" }
1;
