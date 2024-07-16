# Usage:
# docker run -itd --privileged=true -v /etc/cni/net.d:/etc/cni/net.d -v /opt/cni/bin:/opt/cni/bin -v /mnt:/mnt -v /sys/fs/bpf:/sys/fs/bpf -v /lib/modules:/lib/modules --name kmesh kmesh:latest
#
FROM openeuler/openeuler:23.09

WORKDIR /kmesh

RUN \
    --mount=type=cache,target=/var/cache/dnf \
    yum install -y kmod util-linux iptables && \
    mkdir -p /usr/share/oncn-mda && \
    mkdir -p /etc/oncn-mda

COPY out/*so* /usr/lib64/
COPY out/*.o /usr/share/oncn-mda/
COPY out/oncn-mda.conf /etc/oncn-mda/
COPY out/kmesh-daemon /usr/bin/
COPY out/kmesh-cni /usr/bin/
COPY out/mdacore /usr/bin/
COPY build/docker/start_kmesh.sh /kmesh
COPY out/ko /kmesh
