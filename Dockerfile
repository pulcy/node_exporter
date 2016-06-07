FROM        alpine:3.4
MAINTAINER  The Prometheus Authors <prometheus-developers@googlegroups.com>

COPY node_exporter /bin/node_exporter

EXPOSE      9100
ENTRYPOINT  [ "/bin/node_exporter" ]
