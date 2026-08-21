package ops

import (
	"context"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/zap-proto/zip"

	"github.com/hanzoai/k8s/plane"
)

// createJob runs one batch Job and answers 201.
//
// The input is a JOB, not a PodSpec: an image, a command, environment, a service
// account, labels, and claims to mount. That is the whole of what the fleet's job
// callers need — an image build, a migration, a scheduled task — and it is deliberately
// not the Kubernetes PodSpec, because accepting a PodSpec would mean accepting
// hostPath mounts, host networking, privileged security contexts and secret volume
// references, i.e. everything a bounded surface exists to exclude.
//
// `env` is PLAIN environment. Secret material must not travel here: seal it in KMS and
// give the job a ServiceAccount that can reach what it needs. `ttlSeconds` is worth
// setting on anything periodic — a namespace that keeps every finished Job becomes a
// list nobody can read.
//
// Example: {"cluster":"hanzo-k8s","namespace":"hanzo","name":"build-1a2b","image":"gcr.io/kaniko-project/executor:latest","args":["--context=git://…","--destination=oci.hanzo.ai/acme/api:1.4.2"],"ttlSeconds":3600}
// Response: {"namespace":"hanzo","name":"build-1a2b","active":1,"succeeded":0,"failed":0,"phase":"running"}
func (o Ops) createJob(ctx context.Context, in *plane.JobIn) (*plane.Job, error) {
	if err := need("name", in.Name); err != nil {
		return nil, err
	}
	if err := need("image", in.Image); err != nil {
		return nil, err
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	ns, err := b.One(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	labels := map[string]string{"managed-by": managedBy}
	for k, v := range in.Labels {
		labels[k] = v
	}
	env := make([]corev1.EnvVar, 0, len(in.Env))
	for k, v := range in.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	container := corev1.Container{
		Name: "main", Image: in.Image, Command: in.Command, Args: in.Args, Env: env,
	}
	var volumes []corev1.Volume
	for i, m := range in.Mounts {
		if m.Claim == "" || m.Path == "" {
			return nil, zip.ErrBadRequest("every mount needs both a claim and a path")
		}
		name := volumeName(i)
		volumes = append(volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.Claim},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: name, MountPath: m.Path})
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: ns, Labels: labels},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: in.ServiceAccount,
					Containers:         []corev1.Container{container},
					Volumes:            volumes,
				},
			},
		},
	}
	if in.BackoffLimit > 0 {
		limit := in.BackoffLimit
		job.Spec.BackoffLimit = &limit
	}
	if in.TTLSeconds > 0 {
		ttl := in.TTLSeconds
		job.Spec.TTLSecondsAfterFinished = &ttl
	}
	created, err := b.Typed.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fail(err)
	}
	v := jobView(*created)
	return &v, nil
}

// listJobs returns batch Jobs with a rolled-up phase.
//
// `phase` is one word — running, succeeded or failed — computed here rather than left
// to every caller to derive from three counters. Three consumers deriving it three ways
// is three chances to read a failed job as merely unfinished.
//
// Response: {"jobs":[{"namespace":"hanzo","name":"build-1a2b","active":0,"succeeded":1,"failed":0,"phase":"succeeded","startedAt":1785110400,"endedAt":1785110460}]}
func (o Ops) listJobs(ctx context.Context, in *plane.Selector) (*plane.Jobs, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := []plane.Job{}
	for _, ns := range scope {
		list, err := b.Typed.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			out = append(out, jobView(list.Items[i]))
		}
	}
	return &plane.Jobs{Jobs: out}, nil
}

// getJob returns one batch Job.
//
// Response: {"namespace":"hanzo","name":"build-1a2b","active":0,"succeeded":1,"failed":0,"phase":"succeeded","startedAt":1785110400,"endedAt":1785110460}
func (o Ops) getJob(ctx context.Context, in *plane.NamedIn) (*plane.Job, error) {
	if err := need("name", in.Name); err != nil {
		return nil, err
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	ns, err := b.One(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	j, err := b.Typed.BatchV1().Jobs(ns).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fail(err)
	}
	v := jobView(*j)
	return &v, nil
}

func jobView(j batchv1.Job) plane.Job {
	v := plane.Job{
		Namespace: j.Namespace, Name: j.Name,
		Active: j.Status.Active, Succeeded: j.Status.Succeeded, Failed: j.Status.Failed,
		Phase: "running",
	}
	// A completed condition is authoritative over the counters, which is why it is read
	// first: a Job whose pod succeeded and was then garbage-collected has zero of
	// everything, and reading the counters alone would report it as still running.
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			v.Phase = "succeeded"
		case batchv1.JobFailed:
			v.Phase = "failed"
		}
	}
	if v.Phase == "running" && j.Status.Failed > 0 && j.Status.Active == 0 && j.Status.Succeeded == 0 {
		v.Phase = "failed"
	}
	if j.Status.StartTime != nil {
		v.StartedAt = j.Status.StartTime.Unix()
	}
	if j.Status.CompletionTime != nil {
		v.EndedAt = j.Status.CompletionTime.Unix()
	}
	return v
}

// volumeName names a job's Nth mount. Derived from the INDEX rather than from the
// claim, so two claims whose names collide after DNS normalisation still get distinct
// volumes, and stable so a re-created job mounts the same way.
func volumeName(i int) string {
	return "mount-" + strconv.Itoa(i)
}
