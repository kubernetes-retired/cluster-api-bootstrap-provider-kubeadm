# How to release this project

1. Create an annotated tag `git tag -a v0.x.y -m v0.x.y`
    1. To use your GPG signature when pushing the tag, use `git tag -s [...]` instead
1. Push the tag to the GitHub repository `git push origin v0.x.y`
    1. NB: `origin` should be the name of the remote pointing to
       `github.com/kubernetes-sigs/cluster-api-bootstrap-provider-kubeadm`
1. Run `make release` to build artifacts (the image is automatically built by CI)
1. Follow the [Image Promotion process](https://github.com/kubernetes/k8s.io/tree/master/k8s.gcr.io#image-promoter) to
   promote the image from the staging repo to `us.gcr.io/k8s-artifacts-prod/capi-kubeadm`
1. Create a release in GitHub based on the tag created above
    1. Attach `out/bootstrap-components.yaml` to the release
1. Send notifications to the relevant Slack channels and mailing lists
