package aws

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/emr"
	"github.com/patrickmn/go-cache"
	"strings"
	"time"
	"trino-exporter/trino"
)

type ClusterProvider struct {
	emrClient *emr.EMR
	ec2Client *ec2.EC2
	cache     *cache.Cache
}

func NewClusterProvider() *ClusterProvider {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	return &ClusterProvider{
		emrClient: emr.New(sess),
		ec2Client: ec2.New(sess),
		cache:     cache.New(60*time.Minute, 24*time.Hour),
	}
}

const cacheKey = "master"

func (c *ClusterProvider) Provide() (map[string]trino.ClusterInfo, error) {
	result, cached := c.cache.Get(cacheKey)
	if cached {
		return result.(map[string]trino.ClusterInfo), nil
	}

	masters, err := c.listTargetMasters(context.Background())
	if err != nil {
		return nil, err
	}

	c.cache.Set(cacheKey, masters, 30*time.Minute)

	return masters, nil
}

func (c *ClusterProvider) listTargetMasters(ctx context.Context) (map[string]trino.ClusterInfo, error) {

	clusterWithMaster := make(map[string]trino.ClusterInfo)

	clusters, err := c.listTargetClusters(ctx)

	if err != nil {
		return nil, err
	}

	for _, cluster := range clusters {
		master, err := c.getClusterMasterInstance(cluster)
		if err != nil {
			return nil, err
		}

		clusterWithMaster[*cluster.Cluster.Name] = trino.ClusterInfo{
			Host: fmt.Sprintf("http://%s:8889", master),
		}
	}

	return clusterWithMaster, nil
}

func (c *ClusterProvider) listTargetClusters(ctx context.Context) ([]*emr.DescribeClusterOutput, error) {
	req := &emr.ListClustersInput{
		ClusterStates: aws.StringSlice([]string{"WAITING"}),
	}

	clusters := make([]*emr.DescribeClusterOutput, 0)
	err := c.emrClient.ListClustersPagesWithContext(ctx, req, func(output *emr.ListClustersOutput, b bool) bool {

		for _, cluster := range output.Clusters {

			descr, _ := c.emrClient.DescribeCluster(&emr.DescribeClusterInput{
				ClusterId: cluster.Id,
			})

			if !isTrinoInstalled(descr) {
				continue
			}

			clusters = append(clusters, descr)

		}
		return true
	})

	return clusters, err
}

func (c *ClusterProvider) getClusterMasterInstance(cluster *emr.DescribeClusterOutput) (string, error) {

	instanceCollectionType := cluster.Cluster.InstanceCollectionType

	if *instanceCollectionType == emr.InstanceCollectionTypeInstanceGroup {
		return c.getMasterInstanceForNodeGroup(cluster)
	} else if *instanceCollectionType == emr.InstanceCollectionTypeInstanceFleet {
		return c.getMasterInstanceForFleet(cluster)
	}

	return "", fmt.Errorf("unrecognized instance type %s", *instanceCollectionType)
}

func (c *ClusterProvider) getMasterInstanceForFleet(cluster *emr.DescribeClusterOutput) (string, error) {

	instances, err := c.emrClient.ListInstances(&emr.ListInstancesInput{
		ClusterId:         cluster.Cluster.Id,
		InstanceFleetType: aws.String(emr.InstanceFleetTypeMaster),
	})

	if err != nil {
		return "", err
	}

	if len(instances.Instances) == 0 {
		return "", fmt.Errorf("no master instance found for cluster %s", *cluster.Cluster.Id)
	}

	return *instances.Instances[0].PrivateIpAddress, nil
}

func (c *ClusterProvider) getMasterInstanceForNodeGroup(cluster *emr.DescribeClusterOutput) (string, error) {

	instanceGroups, err := c.emrClient.ListInstances(&emr.ListInstancesInput{
		ClusterId:          cluster.Cluster.Id,
		InstanceGroupTypes: []*string{aws.String(emr.InstanceGroupTypeMaster)},
	})

	if err != nil {
		return "", err
	}

	for _, group := range instanceGroups.Instances {

		instances, err := c.emrClient.ListInstances(&emr.ListInstancesInput{
			ClusterId:       cluster.Cluster.Id,
			InstanceGroupId: group.Id,
		})

		if err != nil {
			return "", err
		}

		if len(instances.Instances) == 0 {
			continue
		}

		return *instances.Instances[0].PrivateIpAddress, nil
	}

	return "", fmt.Errorf("no master instance found for cluster %s", *cluster.Cluster.Id)
}

func isTrinoInstalled(descr *emr.DescribeClusterOutput) bool {
	for _, application := range descr.Cluster.Applications {
		if strings.ToLower(*application.Name) == "trino" || strings.ToLower(*application.Name) == "trinodb" {
			return true
		}
	}
	return false
}
