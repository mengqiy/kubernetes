/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/kubectl"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/util/i18n"
)

var (
	replace_long = templates.LongDesc(`
		Replace a resource by filename or stdin.

		JSON and YAML formats are accepted. If replacing an existing resource, the
		complete resource spec must be provided. This can be obtained by

		    $ kubectl get TYPE NAME -o yaml

		Please refer to the models in https://htmlpreview.github.io/?https://github.com/kubernetes/kubernetes/blob/HEAD/docs/api-reference/v1/definitions.html to find if a field is mutable.`)

	replace_example = templates.Examples(`
		# Replace a pod using the data in pod.json.
		kubectl replace -f ./pod.json

		# Replace a pod based on the JSON passed into stdin.
		cat pod.json | kubectl replace -f -

		# Update a single-container pod's image version (tag) to v4
		kubectl get pod mypod -o yaml | sed 's/\(image: myimage\):.*$/\1:v4/' | kubectl replace -f -

		# Replace a deployment using the data in deployment.json and save the config in its annotation.
		kubectl replace -f ./deployment.json --save-config

		# Force replace, delete and then re-create the resource
		kubectl replace --force -f ./pod.json`)
)

func NewCmdReplace(f cmdutil.Factory, out io.Writer) *cobra.Command {
	options := &resource.FilenameOptions{}

	cmd := &cobra.Command{
		Use: "replace -f FILENAME",
		// update is deprecated.
		Aliases: []string{"update"},
		Short:   i18n.T("Replace a resource by filename or stdin"),
		Long:    replace_long,
		Example: replace_example,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(cmdutil.ValidateOutputArgs(cmd))
			err := RunReplace(f, out, cmd, args, options)
			cmdutil.CheckErr(err)
		},
	}
	usage := "to use to replace the resource."
	cmdutil.AddFilenameOptionFlags(cmd, options, usage)
	cmd.MarkFlagRequired("filename")
	cmd.Flags().Bool("force", false, "Delete and re-create the specified resource")
	cmd.Flags().Bool("cascade", false, "Only relevant during a force replace. If true, cascade the deletion of the resources managed by this resource (e.g. Pods created by a ReplicationController).")
	cmd.Flags().Int("grace-period", -1, "Only relevant during a force replace. Period of time in seconds given to the old resource to terminate gracefully. Ignored if negative.")
	cmd.Flags().Duration("timeout", 0, "Only relevant during a force replace. The length of time to wait before giving up on a delete of the old resource, zero means determine a timeout from the size of the object. Any other values should contain a corresponding time unit (e.g. 1s, 2m, 3h).")
	cmdutil.AddValidateFlags(cmd)
	cmdutil.AddOutputFlagsForMutation(cmd)
	overrideMsg := "If true, the configuration of current object will be saved in its annotation. If false and the user-provided config doesn't have the annotation, we will save the annotation from the live object if there are any. If false and the user-provided config has the annotation, we will use the user-provided annotation. This flag is useful when you want to perform kubectl apply on this object in the future."
	cmdutil.AddApplyAnnotationFlagsWithMsg(cmd, overrideMsg)
	cmdutil.AddRecordFlag(cmd)
	cmdutil.AddInclude3rdPartyFlags(cmd)

	return cmd
}

func RunReplace(f cmdutil.Factory, out io.Writer, cmd *cobra.Command, args []string, options *resource.FilenameOptions) error {
	if len(os.Args) > 1 && os.Args[1] == "update" {
		printDeprecationWarning("replace", "update")
	}
	schema, err := f.Validator(cmdutil.GetFlagBool(cmd, "validate"), cmdutil.GetFlagString(cmd, "schema-cache-dir"))
	if err != nil {
		return err
	}

	cmdNamespace, enforceNamespace, err := f.DefaultNamespace()
	if err != nil {
		return err
	}

	force := cmdutil.GetFlagBool(cmd, "force")
	if cmdutil.IsFilenameEmpty(options.Filenames) {
		return cmdutil.UsageError(cmd, "Must specify --filename to replace")
	}

	shortOutput := cmdutil.GetFlagString(cmd, "output") == "name"
	if force {
		return forceReplace(f, out, cmd, args, shortOutput, options)
	}

	if cmdutil.GetFlagInt(cmd, "grace-period") >= 0 {
		return fmt.Errorf("--grace-period must have --force specified")
	}

	if cmdutil.GetFlagDuration(cmd, "timeout") != 0 {
		return fmt.Errorf("--timeout must have --force specified")
	}

	mapper, typer, err := f.UnstructuredObject()
	if err != nil {
		return err
	}
	r := resource.NewBuilder(mapper, typer, resource.ClientMapperFunc(f.UnstructuredClientForMapping), unstructured.UnstructuredJSONScheme).
		Schema(schema).
		ContinueOnError().
		NamespaceParam(cmdNamespace).DefaultNamespace().
		FilenameParam(enforceNamespace, options).
		Flatten().
		Do()
	err = r.Err()
	if err != nil {
		return err
	}

	return r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		saveConfig := cmdutil.GetFlagBool(cmd, cmdutil.ApplyAnnotationsFlag)
		lcl, err := newGetResources(false, nil)
		if err != nil {
			return cmdutil.AddSourceToErr("replacing", info.Source, err)
		}
		if err := handleApplyAnnotation(info, saveConfig, lcl, f.JSONEncoder()); err != nil {
			return cmdutil.AddSourceToErr("replacing", info.Source, err)
		}

		if cmdutil.ShouldRecord(cmd, info) {
			if err := cmdutil.RecordChangeCause(info.Object, f.Command()); err != nil {
				return cmdutil.AddSourceToErr("replacing", info.Source, err)
			}
		}

		// Serialize the object with the annotation applied.
		obj, err := resource.NewHelper(info.Client, info.Mapping).Replace(info.Namespace, info.Name, true, info.Object)
		if err != nil {
			return cmdutil.AddSourceToErr("replacing", info.Source, err)
		}

		info.Refresh(obj, true)
		f.PrintObjectSpecificMessage(obj, out)
		cmdutil.PrintSuccess(mapper, shortOutput, out, info.Mapping.Resource, info.Name, false, "replaced")
		return nil
	})
}

func forceReplace(f cmdutil.Factory, out io.Writer, cmd *cobra.Command, args []string, shortOutput bool, options *resource.FilenameOptions) error {
	schema, err := f.Validator(cmdutil.GetFlagBool(cmd, "validate"), cmdutil.GetFlagString(cmd, "schema-cache-dir"))
	if err != nil {
		return err
	}

	cmdNamespace, enforceNamespace, err := f.DefaultNamespace()
	if err != nil {
		return err
	}

	for i, filename := range options.Filenames {
		if filename == "-" {
			tempDir, err := ioutil.TempDir("", "kubectl_replace_")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tempDir)
			tempFilename := filepath.Join(tempDir, "resource.stdin")
			err = cmdutil.DumpReaderToFile(os.Stdin, tempFilename)
			if err != nil {
				return err
			}
			options.Filenames[i] = tempFilename
		}
	}

	mapper, typer, err := f.UnstructuredObject()
	if err != nil {
		return err
	}

	r := resource.NewBuilder(mapper, typer, resource.ClientMapperFunc(f.UnstructuredClientForMapping), unstructured.UnstructuredJSONScheme).
		ContinueOnError().
		NamespaceParam(cmdNamespace).DefaultNamespace().
		FilenameParam(enforceNamespace, options).
		ResourceTypeOrNameArgs(false, args...).RequireObject(false).
		Latest().
		Flatten().
		Do().
		IgnoreErrors(errors.IsNotFound)
	err = r.Err()
	if err != nil {
		return err
	}
	liveInfos, err := r.Infos()
	if err != nil {
		return err
	}

	r = resource.NewBuilder(mapper, typer, resource.ClientMapperFunc(f.UnstructuredClientForMapping), unstructured.UnstructuredJSONScheme).
		ContinueOnError().
		NamespaceParam(cmdNamespace).DefaultNamespace().
		FilenameParam(enforceNamespace, options).
		ResourceTypeOrNameArgs(false, args...).RequireObject(false).
		Flatten().
		Do()
	err = r.Err()
	if err != nil {
		return err
	}

	//Replace will create a resource if it doesn't exist already, so ignore not found error
	ignoreNotFound := true
	timeout := cmdutil.GetFlagDuration(cmd, "timeout")
	gracePeriod := cmdutil.GetFlagInt(cmd, "grace-period")
	waitForDeletion := false
	if gracePeriod == 0 {
		// To preserve backwards compatibility, but prevent accidental data loss, we convert --grace-period=0
		// into --grace-period=1 and wait until the object is successfully deleted.
		gracePeriod = 1
		waitForDeletion = true
	}
	// By default use a reaper to delete all related resources.
	if cmdutil.GetFlagBool(cmd, "cascade") {
		glog.Warningf("\"cascade\" is set, kubectl will delete and re-create all resources managed by this resource (e.g. Pods created by a ReplicationController). Consider using \"kubectl rolling-update\" if you want to update a ReplicationController together with its Pods.")
		err = ReapResult(r, f, out, cmdutil.GetFlagBool(cmd, "cascade"), ignoreNotFound, timeout, gracePeriod, waitForDeletion, shortOutput, mapper, false)
	} else {
		err = DeleteResult(r, out, ignoreNotFound, shortOutput, mapper)
	}
	if err != nil {
		return err
	}

	if timeout == 0 {
		timeout = kubectl.Timeout
	}
	r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		return wait.PollImmediate(kubectl.Interval, timeout, func() (bool, error) {
			if err := info.Get(); !errors.IsNotFound(err) {
				return false, err
			}
			return true, nil
		})
	})

	r = resource.NewBuilder(mapper, typer, resource.ClientMapperFunc(f.UnstructuredClientForMapping), unstructured.UnstructuredJSONScheme).
		Schema(schema).
		ContinueOnError().
		NamespaceParam(cmdNamespace).DefaultNamespace().
		FilenameParam(enforceNamespace, options).
		Flatten().
		Do()
	err = r.Err()
	if err != nil {
		return err
	}

	count := 0
	err = r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		saveConfig := cmdutil.GetFlagBool(cmd, cmdutil.ApplyAnnotationsFlag)
		lcl, err := newGetResources(true, liveInfos)
		if err != nil {
			return cmdutil.AddSourceToErr("replacing", info.Source, err)
		}
		if err := handleApplyAnnotation(info, saveConfig, lcl, f.JSONEncoder()); err != nil {
			return cmdutil.AddSourceToErr("replacing", info.Source, err)
		}

		if cmdutil.ShouldRecord(cmd, info) {
			if err := cmdutil.RecordChangeCause(info.Object, f.Command()); err != nil {
				return cmdutil.AddSourceToErr("replacing", info.Source, err)
			}
		}

		obj, err := resource.NewHelper(info.Client, info.Mapping).Create(info.Namespace, true, info.Object)
		if err != nil {
			return err
		}

		count++
		info.Refresh(obj, true)
		f.PrintObjectSpecificMessage(obj, out)
		cmdutil.PrintSuccess(mapper, shortOutput, out, info.Mapping.Resource, info.Name, false, "replaced")
		return nil
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("no objects passed to replace")
	}
	return nil
}

// If saveConfig is true, create or update the annotation.
// If saveConfig is false and the local config file doesn't have the annotation, we save the annotation from the live object if there is one.
// If saveConfig is false and the local config file has the annotation, we use the annotation in the config file.
// info is the resource need to process. saveConfig indicate if we want to save the config in the annotation.
func handleApplyAnnotation(info *resource.Info, saveConfig bool, lcl lastConfigurationLookup, encoder runtime.Encoder) error {
	if saveConfig {
		if err := kubectl.CreateOrUpdateAnnotation(true, info, encoder); err != nil {
			return err
		}
		return nil
	}

	anno, err := kubectl.GetOriginalConfiguration(info.Mapping, info.Object)
	if err != nil {
		return err
	}
	// The user-provided config doesn't have the annotation, try to get it from live object
	if anno == nil {
		annotationFromLiveObj, err := lcl.getAnnotationForResource(info)
		if err != nil {
			return err
		}
		if err = kubectl.SetOriginalConfiguration(info, annotationFromLiveObj); err != nil {
			return err
		}
	}
	return nil
}

// newGetResources is the factory method returning an object that implements lastConfigurationLookup interface
func newGetResources(force bool, infos []*resource.Info) (lastConfigurationLookup, error) {
	if force {
		return buildMap(infos)
	} else {
		return lazyResources{}, nil
	}
}

type lastConfigurationLookup interface {
	getAnnotationForResource(info *resource.Info) ([]byte, error)
}

// lazyResources takes a list of resource that will be lazily read from the apiserver when they are needed.
type lazyResources struct{}

// getAnnotationForResource returns the annotation by fetching from the api server.
func (lr lazyResources) getAnnotationForResource(info *resource.Info) ([]byte, error) {
	// save the the local obj before the live obj overwrite it
	localObj := info.Object
	err := info.Get()
	// If the resource is not found in the server, ignore the error
	switch {
	case errors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	anno, err := kubectl.GetOriginalConfiguration(info.Mapping, info.Object)
	// restore the local obj
	info.Object = localObj
	return anno, err
}

// eagerResources takes a list of live resources eagerly read from the apiserver
type eagerResources struct {
	// lookup keeps the mapping of resourceType to the last-applied-config annotation
	lookup map[resourceType][]byte
}

// resourceType is used to check if 2 objects are the same one
type resourceType struct {
	ApiVersion string
	Kind       string
	Name       string
}

// buildMap constructs the mapping(resourceType->annotation) for all the objects in infos
func buildMap(infos []*resource.Info) (eagerResources, error) {
	er := eagerResources{}
	er.lookup = make(map[resourceType][]byte)
	for _, info := range infos {
		rt, err := getResourceTypeForInfo(info)
		if err != nil {
			return er, err
		}
		anno, err := kubectl.GetOriginalConfiguration(info.Mapping, info.Object)
		if err != nil {
			return er, err
		}
		er.lookup[rt] = anno
	}
	return er, nil
}

// getAnnotationForResource returns the annotation by looking up the map built earlier.
func (er eagerResources) getAnnotationForResource(info *resource.Info) ([]byte, error) {
	rt, err := getResourceTypeForInfo(info)
	if err != nil {
		return nil, err
	}
	return er.lookup[rt], nil
}

// getResourceTypeForInfo constructs a resourceType from info
func getResourceTypeForInfo(info *resource.Info) (resourceType, error) {
	accessor := info.Mapping.MetadataAccessor
	apiVersion, err := accessor.APIVersion(info.Object)
	if err != nil {
		return resourceType{}, err
	}
	kind, err := accessor.Kind(info.Object)
	if err != nil {
		return resourceType{}, err
	}
	name, err := accessor.Name(info.Object)
	if err != nil {
		return resourceType{}, err
	}
	return resourceType{
		ApiVersion: apiVersion,
		Kind:       kind,
		Name:       name,
	}, nil
}
