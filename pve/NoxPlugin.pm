package PVE::Storage::Custom::NoxPlugin;

# Proxmox VE storage plugin for a nox shared block pool: one iSCSI/FC LUN,
# sanlock leases, device-mapper volumes. All real work happens in noxd; this
# module only translates PVE storage calls into requests on its Unix socket.

use strict;
use warnings;

use IO::Socket::UNIX;
use JSON::PP;

use PVE::Tools;
use PVE::Storage::Plugin;

use base qw(PVE::Storage::Plugin);

my $DEFAULT_SOCKET = '/run/noxd.sock';

# ---- daemon protocol -------------------------------------------------------

my sub nox_call {
    my ($scfg, $req, $timeout) = @_;

    my $path = $scfg->{'nox-socket'} // $DEFAULT_SOCKET;
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

my sub volume_name {
    my ($class, $volname) = @_;
    my (undef, $name) = $class->parse_volname($volname);
    return $name;
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
        format => [{ raw => 1 }, 'raw'],
        'sensitive-properties' => {},
    };
}

sub properties {
    return {
        'nox-socket' => {
            description => "Control socket of the noxd daemon on each node.",
            type => 'string',
            optional => 1,
        },
    };
}

sub options {
    return {
        'nox-socket' => { optional => 1 },
        nodes => { optional => 1 },
        shared => { optional => 1 },
        disable => { optional => 1 },
        content => { optional => 1 },
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

    if ($volname =~ m/^(vm-(\d+)-\S+)$/) {
        return ('images', $1, $2, undef, undef, undef, 'raw');
    }
    die "unable to parse nox volume name '$volname'\n";
}

sub filesystem_path {
    my ($class, $scfg, $volname, $snapname) = @_;

    die "nox does not support snapshots\n" if defined($snapname);
    my ($vtype, $name, $vmid) = $class->parse_volname($volname);
    my $path = "/dev/mapper/nox-$name";
    return wantarray ? ($path, $vmid, $vtype) : $path;
}

sub qemu_blockdev_options {
    my ($class, $scfg, $storeid, $volname, $machine_version, $options) = @_;

    die "nox does not support snapshots\n" if $options->{'snapshot-name'};
    my ($path) = $class->path($scfg, $volname, $storeid);
    return { driver => 'host_device', filename => $path };
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

# ---- volumes ---------------------------------------------------------------

sub list_images {
    my ($class, $storeid, $scfg, $vmid, $vollist, $cache) = @_;

    $cache->{"nox-list-$storeid"} //= nox_call($scfg, { op => 'list' }, 30)->{volumes};

    my $res = [];
    for my $v (@{ $cache->{"nox-list-$storeid"} }) {
        my $name = $v->{name};
        next if $name !~ m/^vm-(\d+)-/;
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
                format => 'raw',
                size => $v->{size},
                vmid => $owner,
                ctime => $v->{created},
            };
    }
    return $res;
}

sub alloc_image {
    my ($class, $storeid, $scfg, $vmid, $fmt, $name, $size) = @_;    # $size in KiB

    die "unsupported format '$fmt' - nox volumes are raw\n" if $fmt ne 'raw';
    die "illegal name '$name' - should be 'vm-$vmid-*'\n" if $name && $name !~ m/^vm-$vmid-/;
    $name = $class->find_free_diskname($storeid, $scfg, $vmid, $fmt) if !$name;

    my $bytes = $size * 1024;
    $bytes = 4 * 1024 * 1024 if $bytes < 4 * 1024 * 1024;
    nox_call($scfg, { op => 'create', name => $name, size => $bytes }, 60);
    return $name;
}

sub free_image {
    my ($class, $storeid, $scfg, $volname, $isBase, $format) = @_;

    my $name = volume_name($class, $volname);
    eval { nox_call($scfg, { op => 'deactivate', name => $name }, 60) };
    nox_call($scfg, { op => 'delete', name => $name }, 120);
    return undef;
}

sub volume_size_info {
    my ($class, $scfg, $storeid, $volname, $timeout) = @_;

    my $name = volume_name($class, $volname);
    my $v = nox_call($scfg, { op => 'info', name => $name }, $timeout // 30)->{volume};
    return wantarray ? ($v->{size}, 'raw', 0, undef) : $v->{size};
}

sub volume_resize {
    my ($class, $scfg, $storeid, $volname, $size, $running) = @_;    # $size in bytes

    my $name = volume_name($class, $volname);
    nox_call($scfg, { op => 'resize', name => $name, size => int($size) }, 120);
    return 1;
}

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
        next if $conf->{$key} !~ m/(?:^|,|=)\Q$storeid\E:(vm-\d+-[^,\s]+)/;
        push @$names, $1;
    }
    return $names;
}

# The migration target is started with PVE_MIGRATED_FROM set; that is the
# only case where two hosts may legitimately hold the same volume, so ask
# for a shared lease there and an exclusive one everywhere else. noxd
# upgrades shared back to exclusive on its own once the peer releases.
sub activate_volume {
    my ($class, $storeid, $scfg, $volname, $snapname, $cache, $hints) = @_;

    die "nox does not support snapshots\n" if defined($snapname);
    my ($vtype, $name, $vmid) = $class->parse_volname($volname);
    my $mode = $ENV{PVE_MIGRATED_FROM} ? 'sh' : 'ex';
    # Exclusive must outlast sanlock's dead-host detection (about 140 s with
    # io_timeout 10) so a VM can be restarted after its node died.
    my $timeout = $mode eq 'sh' ? 180 : 200;

    # PVE activates one volume at a time, but every nox volume of the guest
    # will be needed shared. Ask for all of them up front so the peer converts
    # them in one lease renewal cycle instead of one cycle per disk.
    if ($mode eq 'sh' && !$cache->{"nox-prepared-$storeid-$vmid"}) {
        $cache->{"nox-prepared-$storeid-$vmid"} = 1;
        my $names = eval { guest_volume_names($storeid, $vmid, $ENV{PVE_MIGRATED_FROM}) } // [];
        eval { nox_call($scfg, { op => 'prepare', names => $names }, 20) } if @$names;
    }

    nox_call($scfg, { op => 'activate', name => $name, mode => $mode, timeout => $timeout }, $timeout + 15);
    return 1;
}


sub deactivate_volume {
    my ($class, $storeid, $scfg, $volname, $snapname, $cache) = @_;

    return 1 if defined($snapname);
    my $name = volume_name($class, $volname);
    nox_call($scfg, { op => 'deactivate', name => $name }, 60);
    return 1;
}

sub volume_has_feature {
    my ($class, $scfg, $feature, $storeid, $volname, $snapname, $running) = @_;

    my $features = {
        copy => { current => 1 },
    };
    my (undef, undef, undef, undef, undef, $isBase) = $class->parse_volname($volname);
    my $key = $snapname ? 'snap' : ($isBase ? 'base' : 'current');
    return 1 if $features->{$feature}->{$key};
    return undef;
}

sub volume_snapshot {
    die "nox does not support snapshots\n";
}

sub volume_snapshot_rollback {
    die "nox does not support snapshots\n";
}

sub volume_snapshot_delete {
    die "nox does not support snapshots\n";
}

sub create_base {
    die "nox does not support templates yet\n";
}

sub clone_image {
    die "nox does not support linked clones\n";
}

sub rename_volume {
    die "nox does not support renaming volumes yet\n";
}

1;
