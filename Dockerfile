# Built by goreleaser (dockers_v2) from the binaries it compiled, one per
# platform. For a local image run: mise run release:snapshot
FROM gcr.io/distroless/static-debian13:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/fastmail-mcp /fastmail-mcp
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/fastmail-mcp"]
CMD ["-http", ":8080"]
