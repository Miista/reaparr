# The binary is cross-compiled with Go beforehand (see the CI workflow and the
# README's Development section) and copied in, so building the image never
# compiles anything — and never needs QEMU emulation for the other platform.
FROM gcr.io/distroless/static-debian12
ARG TARGETARCH
WORKDIR /
COPY dist/linux/${TARGETARCH}/reaparr /reaparr
ENTRYPOINT ["/reaparr"]
