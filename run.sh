#!/bin/bash
# Wrapper script to run Gameplay Clipper using local dependencies

if [ "$#" -lt 1 ]; then
    echo "Usage: ./run.sh <input_video.mp4>"
    exit 1
fi

INPUT_VIDEO=$1

# Setup paths to use the portable Go and FFmpeg installed in the .local folder
export PATH=$(pwd)/.local/go/bin:$(pwd)/.local/bin:$PATH

echo "Building the clipper..."
go build -o clipper .
if [ $? -ne 0 ]; then
    echo "Failed to build the project."
    exit 1
fi

echo "Starting Gameplay Clipper for $INPUT_VIDEO..."
# Execute the binary
./clipper "$INPUT_VIDEO"
