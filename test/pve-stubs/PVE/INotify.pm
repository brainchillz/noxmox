# Minimal stand-in for PVE::INotify for `perl -c pve/NoxPlugin.pm`.
package PVE::INotify;
use strict;
use warnings;
sub nodename { die "stub\n" }
1;
