#!/bin/bash
set -eu -o pipefail

function INFO(){
    echo -e "\e[104m\e[97m[INFO]\e[49m\e[39m $@"
}

ROOTLESSKIT="rootlesskit"
IPERF3C="iperf3 -t 60 -c"

function benchmark::iperf3::slirp4netns(){
    INFO "[benchmark:iperf3] slirp4netns ($@)"
    set -x
    $ROOTLESSKIT --net=slirp4netns $@ $IPERF3C 10.0.2.2
    set +x
}

function benchmark::iperf3::vpnkit(){
    INFO "[benchmark:iperf3] vpnkit ($@)"
    set -x
    $ROOTLESSKIT --net=vpnkit $@ $IPERF3C 192.168.65.2
    set +x
}

function benchmark::iperf3::vdeplug_slirp(){
    INFO "[benchmark:iperf3] vdeplug_slirp ($@)"
    set -x
    $ROOTLESSKIT --net=vdeplug_slirp $@ $IPERF3C 10.0.2.2
    set +x
}

function benchmark::iperf3::rootful_veth(){
    INFO "[benchmark:iperf3] rootful_veth ($@) for reference"
    # only --mtu=MTU is supposed as $@
    mtu=$(echo $@ | sed -e s/--mtu=//g)
    set -x
    sudo ip netns add foo
    sudo ip link add foo_veth0 type veth peer name foo_veth1
    sudo ip link set foo_veth1 netns foo
    sudo ip addr add 10.0.42.1/24 dev foo_veth0
    sudo ip -netns foo addr add 10.0.42.2/24 dev foo_veth1
    sudo ip link set dev foo_veth0 mtu $mtu
    sudo ip -netns foo link set dev foo_veth1 mtu $mtu
    sudo ip link set foo_veth0 up
    sudo ip -netns foo link set foo_veth1 up
    sudo ip netns exec foo $IPERF3C 10.0.42.1
    sudo ip link del foo_veth0
    sudo ip netns del foo
    set +x
}

function benchmark::iperf3::main(){
    iperf3 -s > /dev/null &
    iperf3pid=$!
    for mtu in 1500 4000 16384 65520; do
        benchmark::iperf3::slirp4netns --mtu=$mtu
        if [[ $mtu -gt 16424 ]]; then
            INFO "Skipping benchmark::iperf3::vpnkit --mtu=$mtu (MTU greater than 16424 is known not to work for VPNKit)"
        else
            if [[ $mtu -gt 4000 ]]; then
                INFO "Note: MTU greather than 4K might not be effective for VPNKit: https://twitter.com/mugofsoup/status/1017665057738641408"
            fi
            benchmark::iperf3::vpnkit --mtu=$mtu
        fi
        if [[ $mtu -ne 1500 ]]; then
            INFO "Skipping benchmark::iperf3::vdeplug_slirp --mtu=$mtu (non-1500 MTU is not effective for vdeplug_slirp)"
        else
            benchmark::iperf3::vdeplug_slirp --mtu=$mtu
        fi
        benchmark::iperf3::rootful_veth --mtu=$mtu
    done
    kill $iperf3pid
}

function benchmark::iperf3_reverse::socat_slirp4netns(){
    statedir=$(mktemp -d)
    $ROOTLESSKIT --state-dir=$statedir --net=slirp4netns --mtu=65520 --port-driver=socat iperf3 -s > /dev/null &
    rkpid=$!
    # wait for socket to be available
    sleep 3
    rootlessctl="rootlessctl --socket=$statedir/api.sock"
    portid=$($rootlessctl add-ports 127.0.0.1:5201:5201/tcp)
    $rootlessctl list-ports
    $IPERF3C 127.0.0.1
    $rootlessctl remove-ports $portid
    kill $rkpid
}

function benchmark::iperf3_reverse::builtin_slirp4netns(){
    statedir=$(mktemp -d)
    $ROOTLESSKIT --state-dir=$statedir --net=slirp4netns --mtu=65520 --port-driver=builtin iperf3 -s > /dev/null &
    rkpid=$!
    # wait for socket to be available
    sleep 3
    rootlessctl="rootlessctl --socket=$statedir/api.sock"
    portid=$($rootlessctl add-ports 127.0.0.1:5201:5201/tcp)
    $rootlessctl list-ports
    $IPERF3C 127.0.0.1
    $rootlessctl remove-ports $portid
    kill $rkpid
}


function benchmark::iperf3_reverse::main(){
    benchmark::iperf3_reverse::socat_slirp4netns
    benchmark::iperf3_reverse::builtin_slirp4netns
}
benchmark::iperf3::main
benchmark::iperf3_reverse::main
