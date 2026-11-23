# The sidecar is one static binary, so the image needs nothing else.
# Build it with `make sidecar-image`.
FROM scratch
COPY sidecar-linux /sidecar
ENTRYPOINT ["/sidecar"]
