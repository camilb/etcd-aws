FROM scratch
ADD dist/cacert.pem /etc/ssl/ca-bundle.pem
ADD dist/etcd.Linux.x86_64 /bin/etcd2
ADD dist/etcd-aws.Linux.x86_64 /bin/etcd-aws
ENV PATH=/bin
ENV TMPDIR=/
CMD ["/bin/etcd-aws"]