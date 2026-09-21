FROM e2bdev/base:latest

COPY .build/pons-hands-linux-amd64 /usr/local/bin/pons-hands
RUN sudo chmod 0755 /usr/local/bin/pons-hands
