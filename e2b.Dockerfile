FROM e2bdev/base@sha256:4a369f01a820fe5e65f53c2c5727a78899daf86f0541b721097f289559c8b73f

RUN sudo apt-get update \
    && sudo apt-get install -y --no-install-recommends ca-certificates git \
    && sudo rm -rf /var/lib/apt/lists/*

COPY .build/pons-hands-linux-amd64 /usr/local/bin/pons-hands
RUN sudo chmod 0755 /usr/local/bin/pons-hands
