# Minimal stand-in for PVE::Storage::Common so `perl -c pve/NoxPlugin.pm`
# runs on a machine without Proxmox.
package PVE::Storage::Common;
use strict;
use warnings;
sub qemu_img_create              { die "stub\n" }
sub qemu_img_create_qcow2_backed { die "stub\n" }
sub qemu_img_measure             { die "stub\n" }
sub qemu_img_resize              { die "stub\n" }
1;
