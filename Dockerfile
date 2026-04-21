# Stage 1: Build the Go application
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git || true
WORKDIR /app

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -o indexer .

# Stage 2: Build parpar native C++ addon (needs python3 + build tools for node-gyp)
FROM node:22-alpine AS parpar-builder
RUN apk add --no-cache python3 make g++ && \
    npm install -g @animetosho/parpar && \
    npm cache clean --force && \
    parpar --version

# Stage 3: Create the runtime container — use the SAME node base so the
# native addon's NODE_MODULE_VERSION matches the runtime exactly.
FROM node:22-alpine
RUN apk --no-cache add ca-certificates ffmpeg par2cmdline && \
    (apk --no-cache add 7zip || apk --no-cache add p7zip)
WORKDIR /app

# parpar: copy the full package tree from the builder. The npm symlink at
# /usr/local/bin/parpar is relative (../lib/node_modules/@animetosho/parpar/bin/parpar.js)
# so we copy both the modules and recreate the link.
COPY --from=parpar-builder /usr/local/lib/node_modules/@animetosho /usr/local/lib/node_modules/@animetosho
RUN ln -sf /usr/local/lib/node_modules/@animetosho/parpar/bin/parpar.js /usr/local/bin/parpar
COPY --from=builder /app/indexer .

CMD ["./indexer"]
