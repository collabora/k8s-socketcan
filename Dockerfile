FROM scratch
COPY k8s-socketcan /
ENTRYPOINT ["/k8s-socketcan"]
