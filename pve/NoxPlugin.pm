package PVE::Storage::Custom::NoxPlugin;

# Proxmox VE storage plugin for a nox shared block pool: one iSCSI/FC LUN,
# sanlock leases, device-mapper volumes. All real work happens in noxd; this
# module translates PVE storage calls into requests on its Unix socket.
#
# Snapshots follow PVE 9's "snapshot as volume chain" model, exactly like the
# LVM plugin with that option on: a qcow2 volume lives directly on its dm
# device, a snapshot renames the current volume to snap_<name>_<snap>.qcow2
# and allocates a new current volume backed by it. The daemon records the
# backing relation and holds every backing volume of an active chain with a
# shared lease.

use strict;
use warnings;

use IO::Socket::UNIX;
use JSON::PP;

use PVE::INotify;
use PVE::Tools qw(run_command);
use PVE::Storage::Plugin;
use PVE::Storage::Common;

use base qw(PVE::Storage::Plugin);

my $DEFAULT_POOL = 'nox';

# ---- daemon protocol -------------------------------------------------------

my sub nox_call {
    my ($scfg, $req, $timeout) = @_;

    my $path = $scfg->{'nox-socket'} // ('/run/noxd-' . ($scfg->{pool} // $DEFAULT_POOL) . '.sock');
    $timeout //= 60;

    my $resp;
    PVE::Tools::run_with_timeout(
        $timeout,
        sub {
            my $sock = IO::Socket::UNIX->new(Type => SOCK_STREAM(), Peer => $path)
                or die "noxd not reachable at $path: $!\n";
            $sock->autoflush(1);
            print $sock JSON::PP->new->canonical->encode($req), "\n";
            my $line = <$sock>;
            close($sock);
            die "no reply from noxd\n" if !defined($line);
            $resp = JSON::PP->new->decode($line);
        },
    );
    die "noxd: $resp->{error}\n" if !$resp->{ok};
    return $resp;
}

my sub nox_list {
    my ($scfg) = @_;
    return nox_call($scfg, { op => 'list' }, 30)->{volumes};
}

my sub nox_info {
    my ($scfg, $name, $timeout) = @_;
    return nox_call($scfg, { op => 'info', name => $name }, $timeout // 30)->{volume};
}

# Device-mapper names carry the pool name as prefix, like LVM's vg-lv.
my sub dm_prefix {
    my ($scfg) = @_;
    return ($scfg->{pool} // $DEFAULT_POOL) . '-';
}

my sub dm_path {
    my ($scfg, $name) = @_;
    return '/dev/mapper/' . dm_prefix($scfg) . $name;
}

# ---- plugin identity -------------------------------------------------------

sub api {
    return 15;
}

sub type {
    return 'nox';
}

sub plugindata {
    return {
        content => [{ images => 1, rootdir => 1 }, { images => 1 }],
        format => [{ raw => 1, qcow2 => 1 }, 'qcow2'],
        'sensitive-properties' => {},
    };
}

sub get_formats {
    my ($class, $scfg, $storeid) = @_;
    return { default => 'qcow2', valid => { qcow2 => 1, raw => 1 } };
}

sub properties {
    return {
        # 'pool' is a property PVE already defines (for Ceph); it is reused
        # through options() as the nox pool name and must not be redeclared.
        'nox-socket' => {
            description => "Control socket of the noxd daemon on each node (default /run/noxd-<pool>.sock).",
            type => 'string',
            optional => 1,
        },
        'allow-foreign-delete' => {
            description => "Allow deleting volumes that were created by another host and are not"
                . " referenced by a guest on this node. Off by default: on a pool shared by"
                . " unrelated nodes, 'purge unreferenced disks' would otherwise remove another"
                . " node's volumes that happen to carry the same VMID.",
            type => 'boolean',
            optional => 1,
            default => 0,
        },
    };
}

sub options {
    return {
        pool => { optional => 1 },
        'nox-socket' => { optional => 1 },
        'allow-foreign-delete' => { optional => 1 },
        nodes => { optional => 1 },
        shared => { optional => 1 },
        disable => { optional => 1 },
        content => { optional => 1 },
        format => { optional => 1 },
        preallocation => { optional => 1 },
        bwlimit => { optional => 1 },
    };
}

# A nox pool is shared by construction; force the flag so migration never
# tries to copy disks.
sub on_add_hook {
    my ($class, $storeid, $scfg, %param) = @_;
    $scfg->{shared} = 1;
    return;
}

sub on_update_hook {
    my ($class, $storeid, $scfg, %param) = @_;
    $scfg->{shared} = 1;
    return;
}

# ---- names and paths -------------------------------------------------------

sub parse_volname {
    my ($class, $volname) = @_;

    if ($volname =~ m/^((vm|base)-(\d+)-\S+)$/) {
        my ($name, $kind, $vmid) = ($1, $2, $3);
        my $format = $name =~ m/\.qcow2$/ ? 'qcow2' : 'raw';
        return ('images', $name, $vmid, undef, undef, $kind eq 'base', $format);
    }
    die "unable to parse nox volume name '$volname'\n";
}

my sub get_snap_name {
    my ($class, $volname, $snapname) = @_;

    die "missing snapname\n" if !$snapname;
    my (undef, $name) = $class->parse_volname($volname);
    return $name if $snapname eq 'current';
    $name =~ s/\.[^.]+$//;
    return "snap_${name}_${snapname}.qcow2";
}

my sub parse_snap_name {
    my ($name, $short_volname) = @_;

    $short_volname =~ s/\.qcow2$//;
    if ($name =~ m/^snap_\Q$short_volname\E_(.*)\.qcow2$/) {
        return $1;
    }
    return undef;
}

sub filesystem_path {
    my ($class, $scfg, $volname, $snapname) = @_;

    my ($vtype, $name, $vmid, undef, undef, undef, $format) = $class->parse_volname($volname);
    die "snapshots need the qcow2 format\n" if defined($snapname) && $format ne 'qcow2';
    $name = get_snap_name($class, $volname, $snapname) if $snapname;
    my $path = dm_path($scfg, $name);
    return wantarray ? ($path, $vmid, $vtype) : $path;
}

sub qemu_blockdev_options {
    my ($class, $scfg, $storeid, $volname, $machine_version, $options) = @_;

    my ($path) = $class->path($scfg, $volname, $storeid, $options->{'snapshot-name'});
    return { driver => 'host_device', filename => $path };
}

sub find_free_diskname {
    my ($class, $storeid, $scfg, $vmid, $fmt, $add_fmt_suffix) = @_;

    my $disk_list = [map { $_->{name} } @{ nox_list($scfg) }];
    $add_fmt_suffix = $fmt && $fmt eq 'qcow2' ? 1 : undef;
    return PVE::Storage::Plugin::get_next_vm_diskname(
        $disk_list, $storeid, $vmid, $fmt, $scfg, $add_fmt_suffix,
    );
}

# ---- storage lifecycle -----------------------------------------------------

sub activate_storage {
    my ($class, $storeid, $scfg, $cache) = @_;

    my $st = nox_call($scfg, { op => 'status' }, 20);
    die "nox pool is not ready on this node (lockspace not joined)\n" if !$st->{ready};
    return 1;
}

sub deactivate_storage {
    my ($class, $storeid, $scfg, $cache) = @_;
    return 1;
}

sub check_connection {
    my ($class, $storeid, $scfg) = @_;

    my $st = eval { nox_call($scfg, { op => 'status' }, 10) };
    return $st && $st->{ready} ? 1 : 0;
}

sub status {
    my ($class, $storeid, $scfg, $cache) = @_;

    my $st = eval { nox_call($scfg, { op => 'status' }, 10) };
    return if !$st;
    return ($st->{total}, $st->{free}, $st->{used}, $st->{ready} ? 1 : 0);
}

# ---- allocation ------------------------------------------------------------

# Bytes to reserve on the pool for a volume of $size_kib guest visible KiB.
# A qcow2 volume is sized for its fully allocated image, as the LVM plugin
# does, so it can never run out of room under the guest.
my sub reserve_bytes {
    my ($size_kib, $fmt, $backed) = @_;

    return $size_kib * 1024 if $fmt ne 'qcow2';
    my $options = $backed ? ['extended_l2=on', 'cluster_size=128k'] : [];
    my $json = PVE::Storage::Common::qemu_img_measure($size_kib, $fmt, 5, $options);
    die "failed to query file information with qemu-img measure\n" if !$json;
    my $info = eval { decode_json($json) };
    die "Invalid JSON: $@\n" if $@;
    die "Missing fully-allocated value from json" if !$info->{'fully-allocated'};
    return $info->{'fully-allocated'};
}

# Create a volume on the pool and, for qcow2, write the image header on it.
# $backing is the name of the backing volume (a snapshot or a base image) or
# undef. The volume is left active; callers that allocate offline deactivate.
my sub nox_alloc {
    my ($class, $storeid, $scfg, $name, $fmt, $size_kib, $backing, $backing_fmt) = @_;

    die "unsupported format '$fmt'\n" if $fmt ne 'raw' && $fmt ne 'qcow2';
    $class->parse_volname($name);
    my $bytes = reserve_bytes($size_kib, $fmt, defined($backing));
    $bytes = 4 * 1024 * 1024 if $bytes < 4 * 1024 * 1024;

    my $req = { op => 'create', name => $name, size => int($bytes), format => $fmt };
    if ($fmt eq 'qcow2') {
        $req->{virt_size} = int($size_kib * 1024);
        $req->{parent} = $backing if defined($backing);
    }
    nox_call($scfg, $req, 60);
    return if $fmt ne 'qcow2';

    eval {
        $class->activate_volume($storeid, $scfg, $name);
        my $path = dm_path($scfg, $name);
        my $options = { preallocation => PVE::Storage::Plugin::preallocation_cmd_opt($scfg, $fmt) };
        if (defined($backing)) {
            # relative to /dev/mapper, where the top image lives
            PVE::Storage::Common::qemu_img_create_qcow2_backed(
                $path, dm_prefix($scfg) . $backing, $backing_fmt // 'qcow2', $options,
            );
        } else {
            PVE::Storage::Common::qemu_img_create($fmt, $size_kib, $path, $options);
        }
    };
    if (my $err = $@) {
        eval { nox_call($scfg, { op => 'deactivate', name => $name }, 60) };
        eval { nox_call($scfg, { op => 'delete', name => $name }, 60) };
        die $err;
    }
}

# Only qcow2 names carry an extension; add it for callers passing a fixed
# name (cloud-init drives are raw and keep theirs).
my sub volname_for_format {
    my ($class, $name, $fmt) = @_;

    my $name_fmt = ($class->parse_volname($name))[6];
    return $name if $name_fmt eq $fmt;
    if ($fmt eq 'raw') {
        my $suggested = $name =~ s/\.\Q$name_fmt\E$//r;
        die "volume name '$name' does not match requested format '$fmt' (did you mean '$suggested'?)\n";
    }
    return "$name.$fmt";
}

sub alloc_image {
    my ($class, $storeid, $scfg, $vmid, $fmt, $name, $size) = @_;    # $size in KiB

    die "unsupported format '$fmt' - nox volumes are raw or qcow2\n" if $fmt ne 'raw' && $fmt ne 'qcow2';
    $name = $class->find_free_diskname($storeid, $scfg, $vmid, $fmt) if !$name;
    $name = volname_for_format($class, $name, $fmt);
    die "illegal name '$name' - should be 'vm-$vmid-*'\n" if $name !~ m/^vm-$vmid-/;

    nox_alloc($class, $storeid, $scfg, $name, $fmt, $size);
    eval { nox_call($scfg, { op => 'deactivate', name => $name }, 60) } if $fmt eq 'qcow2';
    return $name;
}

# Guests on unrelated nodes may share a VMID. A volume this node did not
# create and no guest on this node references is most likely another node's
# disk; refuse to remove it unless the storage says foreign deletes are fine.
my sub assert_deletable {
    my ($storeid, $scfg, $name, $vmid) = @_;

    return if $scfg->{'allow-foreign-delete'};
    my $v = nox_info($scfg, $name);
    my $me = nox_call($scfg, { op => 'status' }, 10)->{host_id};
    return if !$v->{creator} || !$me || $v->{creator} == $me;
    my $node = PVE::INotify::nodename();
    for my $conf ("/etc/pve/nodes/$node/qemu-server/$vmid.conf", "/etc/pve/nodes/$node/lxc/$vmid.conf") {
        next if !-e $conf;
        my $text = PVE::Tools::file_get_contents($conf);
        return if $text =~ m/\Q$storeid:$name\E(?:[,\s]|$)/;
    }
    die "refusing to delete $storeid:$name: created by host $v->{creator}, not by this node (host $me),"
        . " and no guest $vmid on this node references it. Set allow-foreign-delete on the storage"
        . " if this pool is shared only within one cluster.\n";
}

sub free_image {
    my ($class, $storeid, $scfg, $volname, $isBase, $format) = @_;

    my ($vtype, $name, $vmid) = $class->parse_volname($volname);
    assert_deletable($storeid, $scfg, $name, $vmid);
    my $names = [$name];
    if ($format eq 'qcow2') {
        # top first, then its snapshots down the chain: a backing volume can
        # only go once nothing is backed by it
        my $snapshots = $class->volume_snapshot_info($scfg, $storeid, $volname);
        for my $snapid (sort { $snapshots->{$a}->{order} <=> $snapshots->{$b}->{order} } keys %$snapshots) {
            next if $snapid eq 'current';
            push @$names, $snapshots->{$snapid}->{volname};
        }
    }
    for my $n (@$names) {
        eval { nox_call($scfg, { op => 'deactivate', name => $n }, 60) };
        nox_call($scfg, { op => 'delete', name => $n }, 120);
    }
    return undef;
}

sub list_images {
    my ($class, $storeid, $scfg, $vmid, $vollist, $cache) = @_;

    $cache->{"nox-list-$storeid"} //= nox_list($scfg);

    my $res = [];
    for my $v (@{ $cache->{"nox-list-$storeid"} }) {
        my $name = $v->{name};
        next if $name !~ m/^(?:vm|base)-(\d+)-/;
        my $owner = $1;
        my $volid = "$storeid:$name";

        if ($vollist) {
            next if !grep { $_ eq $volid } @$vollist;
        } else {
            next if defined($vmid) && $owner ne $vmid;
        }

        push @$res,
            {
                volid => $volid,
                format => $v->{format} // 'raw',
                size => $v->{virt_size} // $v->{size},
                vmid => $owner,
                ctime => $v->{created},
            };
    }
    return $res;
}

sub volume_size_info {
    my ($class, $scfg, $storeid, $volname, $timeout) = @_;

    my (undef, $name, undef, undef, undef, undef, $format) = $class->parse_volname($volname);
    my $v = nox_info($scfg, $name, $timeout // 30);
    my $size = $v->{virt_size} // $v->{size};
    return wantarray ? ($size, $format, $v->{size}, undef) : $size;
}

sub volume_resize {
    my ($class, $scfg, $storeid, $volname, $size, $running, $snapname) = @_;    # $size in bytes

    my (undef, $name, undef, undef, undef, undef, $format) = $class->parse_volname($volname);
    $name = get_snap_name($class, $volname, $snapname) if $snapname;

    if ($format eq 'raw') {
        nox_call($scfg, { op => 'resize', name => $name, size => int($size) }, 120);
        return 1;
    }
    my $v = nox_info($scfg, $name);
    my $bytes = reserve_bytes(int($size / 1024), 'qcow2', defined($v->{parent}));
    $bytes = $v->{size} if $bytes < $v->{size};
    nox_call($scfg, { op => 'resize', name => $name, size => int($bytes), virt_size => int($size) }, 120);

    if (!$running) {
        my $was_active = defined($v->{mode});
        $class->activate_volume($storeid, $scfg, $volname, $snapname);
        my $preallocation = PVE::Storage::Plugin::preallocation_cmd_opt($scfg, $format);
        eval { PVE::Storage::Common::qemu_img_resize(dm_path($scfg, $name), $format, $size, $preallocation, 10) };
        my $err = $@;
        $class->deactivate_volume($storeid, $scfg, $volname, $snapname) if !$was_active;
        die $err if $err;
    }
    return 1;
}

# ---- activation ------------------------------------------------------------

# Collect the nox volumes referenced by a guest's config, from whichever node
# currently owns the config (the migration source).
my sub guest_volume_names {
    my ($storeid, $vmid, $node) = @_;

    my $names = [];
    my $conf;
    if (-e "/etc/pve/nodes/$node/qemu-server/$vmid.conf") {
        require PVE::QemuConfig;
        $conf = PVE::QemuConfig->load_config($vmid, $node);
    } elsif (-e "/etc/pve/nodes/$node/lxc/$vmid.conf") {
        require PVE::LXC::Config;
        $conf = PVE::LXC::Config->load_config($vmid, $node);
    }
    return $names if !$conf;
    for my $key (keys %$conf) {
        next if ref($conf->{$key});
        next if $conf->{$key} !~ m/(?:^|,|=)\Q$storeid\E:((?:vm|base)-\d+-[^,\s]+)/;
        push @$names, $1;
    }
    return $names;
}

# The migration target is started with PVE_MIGRATED_FROM set; that is the
# only case where two hosts may legitimately hold the same volume, so ask
# for a shared lease there and an exclusive one everywhere else. noxd
# upgrades shared back to exclusive on its own once the peer releases.
# Base images and snapshots are backing volumes and always shared; the
# daemon activates the backing chain beneath a qcow2 top by itself.
sub activate_volume {
    my ($class, $storeid, $scfg, $volname, $snapname, $cache, $hints) = @_;

    my ($vtype, $name, $vmid, undef, undef, $isBase) = $class->parse_volname($volname);
    $name = get_snap_name($class, $volname, $snapname) if $snapname;
    my $mode = ($ENV{PVE_MIGRATED_FROM} || $isBase || $snapname) ? 'sh' : 'ex';
    # Exclusive must outlast sanlock's dead-host detection (about 140 s with
    # io_timeout 10) so a VM can be restarted after its node died.
    my $timeout = $mode eq 'sh' ? 180 : 200;

    # PVE activates one volume at a time, but every nox volume of the guest
    # will be needed shared. Ask for all of them up front so the peer converts
    # them in one go instead of one request per disk.
    if ($ENV{PVE_MIGRATED_FROM} && !$cache->{"nox-prepared-$storeid-$vmid"}) {
        $cache->{"nox-prepared-$storeid-$vmid"} = 1;
        my $names = eval { guest_volume_names($storeid, $vmid, $ENV{PVE_MIGRATED_FROM}) } // [];
        eval { nox_call($scfg, { op => 'prepare', names => $names }, 20) } if @$names;
    }

    nox_call($scfg, { op => 'activate', name => $name, mode => $mode, timeout => $timeout }, $timeout + 15);
    return 1;
}

sub deactivate_volume {
    my ($class, $storeid, $scfg, $volname, $snapname, $cache) = @_;

    my (undef, $name) = $class->parse_volname($volname);
    $name = get_snap_name($class, $volname, $snapname) if $snapname;
    nox_call($scfg, { op => 'deactivate', name => $name }, 60);
    return 1;
}

# ---- snapshots as volume chains --------------------------------------------

# The chain is read from the daemon's records (every volume knows its
# backing volume), so no image needs to be active to list snapshots.
sub volume_snapshot_info {
    my ($class, $scfg, $storeid, $volname) = @_;

    my (undef, $name, undef, undef, undef, undef, $format) = $class->parse_volname($volname);
    return {} if $format ne 'qcow2';

    my %byname = map { $_->{name} => $_ } @{ nox_list($scfg) };
    my $info = {};
    my $order = 0;
    my $cur = $byname{$name};
    my $childsnap;
    my %seen;
    while ($cur && !$seen{ $cur->{name} }++) {
        my $snapname = $cur->{name} eq $name ? 'current' : parse_snap_name($cur->{name}, $name);
        last if !$snapname;    # a base image or a foreign chain member ends the list
        $info->{$snapname} = {
            order => $order++,
            file => dm_path($scfg, $cur->{name}),
            volname => $cur->{name},
            volid => "$storeid:$cur->{name}",
            'virtual-size' => $cur->{virt_size} // $cur->{size},
        };
        $info->{$childsnap}->{parent} = $snapname if defined($childsnap);
        $info->{$snapname}->{child} = $childsnap if defined($childsnap);
        $childsnap = $snapname;
        $cur = defined($cur->{parent}) ? $byname{ $cur->{parent} } : undef;
    }
    return $info;
}

sub volume_snapshot {
    my ($class, $scfg, $storeid, $volname, $snap) = @_;

    my (undef, $name, undef, undef, undef, undef, $format) = $class->parse_volname($volname);
    die "can't snapshot '$format' volume\n" if $format ne 'qcow2';
    my $snapvol = get_snap_name($class, $volname, $snap);

    my $v = nox_info($scfg, $name);
    my $running = defined($v->{mode});    # held on this node: a guest has it open
    my $size_kib = int(($v->{virt_size} // $v->{size}) / 1024);

    # current becomes the snapshot, a new current goes on top of it
    nox_call($scfg, { op => 'rename', name => $name, new_name => $snapvol }, 60);
    eval { nox_alloc($class, $storeid, $scfg, $name, 'qcow2', $size_kib, $snapvol, 'qcow2') };
    if (my $err = $@) {
        eval { nox_call($scfg, { op => 'rename', name => $snapvol, new_name => $name }, 60) };
        die "error creating snapshot $snap of $volname - $err";
    }
    $class->deactivate_volume($storeid, $scfg, $volname) if !$running;
    return;
}

sub volume_rollback_is_possible {
    my ($class, $scfg, $storeid, $volname, $snap, $blockers) = @_;

    my $format = ($class->parse_volname($volname))[6];
    die "can't rollback snapshot for '$format' volume\n" if $format ne 'qcow2';

    my $snapshots = $class->volume_snapshot_info($scfg, $storeid, $volname);
    die "can't rollback, snapshot '$snap' does not exist on '$volname'\n" if !$snapshots->{$snap};
    $blockers //= [];
    my $parent = $snapshots->{current}->{parent} // '';
    if ($parent ne $snap) {
        for my $s (keys %$snapshots) {
            next if $s eq 'current' || $s eq $snap;
            push @$blockers, $s if $snapshots->{$s}->{order} < $snapshots->{$snap}->{order};
        }
        die "can't rollback, '$snap' is not most recent snapshot on '$volname'\n";
    }
    return 1;
}

sub volume_snapshot_rollback {
    my ($class, $scfg, $storeid, $volname, $snap) = @_;

    my (undef, $name) = $class->parse_volname($volname);
    $class->volume_rollback_is_possible($scfg, $storeid, $volname, $snap);
    my $snapvol = get_snap_name($class, $volname, $snap);
    my $v = nox_info($scfg, $name);
    my $size_kib = int(($v->{virt_size} // $v->{size}) / 1024);

    # discard the current top and start a fresh one over the snapshot
    eval { nox_call($scfg, { op => 'deactivate', name => $name }, 60) };
    nox_call($scfg, { op => 'delete', name => $name }, 120);
    nox_alloc($class, $storeid, $scfg, $name, 'qcow2', $size_kib, $snapvol, 'qcow2');
    $class->deactivate_volume($storeid, $scfg, $volname);
    return undef;
}

sub volume_snapshot_delete {
    my ($class, $scfg, $storeid, $volname, $snap, $running) = @_;

    my (undef, $name, undef, undef, undef, undef, $format) = $class->parse_volname($volname);
    die "can't delete snapshot for '$format' volume\n" if $format ne 'qcow2';
    my $snapshots = $class->volume_snapshot_info($scfg, $storeid, $volname);
    die "snapshot '$snap' does not exist on '$volname'\n" if !$snapshots->{$snap};
    my $snapvol = $snapshots->{$snap}->{volname};

    if ($running) {
        # qemu-server has already committed or streamed the data and detached
        # the node; the record goes and its children move up one link
        eval { nox_call($scfg, { op => 'deactivate', name => $snapvol }, 60) };
        nox_call($scfg, { op => 'delete', name => $snapvol, reparent => JSON::PP::true }, 120);
        return;
    }

    my $parentsnap = $snapshots->{$snap}->{parent};
    my $childsnap = $snapshots->{$snap}->{child};
    my $childvol = $snapshots->{$childsnap}->{volname};
    my $childpath = $snapshots->{$childsnap}->{file};

    # the whole chain comes up with the top; remember whether it was up
    my $was_active = defined(nox_info($scfg, $name)->{mode});
    $class->activate_volume($storeid, $scfg, $volname);
    my $err;
    eval {
        if (!$parentsnap) {
            # first snapshot: merge the child down into it (the base is the
            # big one), drop the child, and give the snapshot the child's name
            print "$volname: deleting snapshot '$snap' by committing '$childsnap' into it\n";
            my $snap_size = $snapshots->{$snap}->{'virtual-size'};
            my $child_size = $snapshots->{$childsnap}->{'virtual-size'};
            if (defined($child_size) && defined($snap_size) && $child_size > $snap_size) {
                $class->volume_resize($scfg, $storeid, $volname, $child_size, 1, $snap);
            }
            run_command(['/usr/bin/qemu-img', 'commit', $childpath], errmsg => "qemu-img commit $childpath failed");
            eval { nox_call($scfg, { op => 'deactivate', name => $childvol }, 60) };
            nox_call($scfg, { op => 'delete', name => $childvol, reparent => JSON::PP::true }, 120);
            nox_call($scfg, { op => 'rename', name => $snapvol, new_name => $childvol }, 60);
        } else {
            # intermediate snapshot: rebase the child onto the grandparent
            my $parentvol = $snapshots->{$parentsnap}->{volname};
            print "$volname: deleting snapshot '$snap' by rebasing '$childsnap' onto '$parentsnap'\n";
            run_command(
                ['/usr/bin/qemu-img', 'rebase', '-b', dm_prefix($scfg) . $parentvol, '-F', 'qcow2', '-f', 'qcow2', $childpath],
                errmsg => "qemu-img rebase $childpath failed",
            );
            eval { nox_call($scfg, { op => 'deactivate', name => $snapvol }, 60) };
            nox_call($scfg, { op => 'delete', name => $snapvol, reparent => JSON::PP::true }, 120);
        }
    };
    $err = $@;
    $class->deactivate_volume($storeid, $scfg, $volname) if !$was_active;
    die $err if $err;
    return;
}

sub rename_snapshot {
    my ($class, $scfg, $storeid, $volname, $source_snap, $target_snap) = @_;

    my $src = get_snap_name($class, $volname, $source_snap);
    my $dst = get_snap_name($class, $volname, $target_snap);
    nox_call($scfg, { op => 'rename', name => $src, new_name => $dst }, 60);
    return;
}

sub volume_qemu_snapshot_method {
    my ($class, $storeid, $scfg, $volname) = @_;

    my $format = ($class->parse_volname($volname))[6];
    return 'mixed' if $format eq 'qcow2';
    return 'storage';
}

# ---- templates, clones, rename ---------------------------------------------

sub create_base {
    my ($class, $storeid, $scfg, $volname) = @_;

    my (undef, $name, undef, undef, undef, $isBase, $format) = $class->parse_volname($volname);
    die "create_base not possible with base image\n" if $isBase;
    if ($format eq 'qcow2') {
        my $snapshots = $class->volume_snapshot_info($scfg, $storeid, $volname);
        die "unable to create base volume - snapshots exist on '$volname'\n" if $snapshots->{current}->{parent};
    }
    my $newname = $name =~ s/^vm-/base-/r;
    eval { nox_call($scfg, { op => 'deactivate', name => $name }, 60) };
    nox_call($scfg, { op => 'rename', name => $name, new_name => $newname }, 60);
    return $newname;
}

# A linked clone is a fresh qcow2 top backed by the base image (or by one
# of the source's snapshots). The daemon holds the base shared on every
# host that runs a clone.
sub clone_image {
    my ($class, $scfg, $storeid, $volname, $vmid, $snap) = @_;

    my (undef, $name, undef, undef, undef, $isBase, $format) = $class->parse_volname($volname);
    my $backing;
    if ($snap) {
        $backing = get_snap_name($class, $volname, $snap);
        $format = 'qcow2';
    } else {
        die "clone_image only works on base images\n" if !$isBase;
        $backing = $name;
    }
    my $src = nox_info($scfg, $backing);
    my $size_kib = int(($src->{virt_size} // $src->{size}) / 1024);
    my $newname = $class->find_free_diskname($storeid, $scfg, $vmid, 'qcow2');
    nox_alloc($class, $storeid, $scfg, $newname, 'qcow2', $size_kib, $backing, $format);
    eval { nox_call($scfg, { op => 'deactivate', name => $newname }, 60) };
    return $newname;
}

sub rename_volume {
    my ($class, $scfg, $storeid, $source_volname, $target_vmid, $target_volname) = @_;

    my (undef, $source_name, $source_vmid, undef, undef, undef, $format) = $class->parse_volname($source_volname);
    if ($format eq 'qcow2') {
        my $snapshots = $class->volume_snapshot_info($scfg, $storeid, $source_volname);
        die "can't rename volume '$source_volname' - snapshots exist\n" if $snapshots->{current}->{parent};
    }
    $target_volname = $class->find_free_diskname($storeid, $scfg, $target_vmid, $format) if !$target_volname;
    nox_call($scfg, { op => 'rename', name => $source_name, new_name => $target_volname }, 60);
    return "${storeid}:${target_volname}";
}

sub volume_has_feature {
    my ($class, $scfg, $feature, $storeid, $volname, $snapname, $running) = @_;

    my $features = {
        snapshot => { current => { qcow2 => 1 }, snap => { qcow2 => 1 } },
        clone => { base => { qcow2 => 1, raw => 1 }, snap => { qcow2 => 1 } },
        template => { current => { qcow2 => 1, raw => 1 } },
        copy => { base => { qcow2 => 1, raw => 1 }, current => { qcow2 => 1, raw => 1 }, snap => { qcow2 => 1 } },
        rename => { current => { qcow2 => 1, raw => 1 } },
    };
    my (undef, undef, undef, undef, undef, $isBase, $format) = $class->parse_volname($volname);
    my $key = $snapname ? 'snap' : ($isBase ? 'base' : 'current');
    return 1 if $features->{$feature}->{$key}->{$format};
    return undef;
}

1;
