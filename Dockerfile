FROM registry.svc.ci.openshift.org/openshift/release:golang-1.10 AS builder
WORKDIR /go/src/github.com/openshift/cluster-autoscaler-operator
COPY . .
ENV NO_DOCKER=1
ENV BUILD_DEST=/go/bin/cluster-autoscaler-operator
RUN unset VERSION && make build

FROM registry.svc.ci.openshift.org/openshift/origin-v4.0:base
COPY --from=builder # /go/bin/cluster-autoscaler-operator /usr/bin/
CMD ["/usr/bin/cluster-autoscaler-operator"]
