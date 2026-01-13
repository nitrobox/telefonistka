package githubapi

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	cfg "github.com/commercetools/telefonistka/internal/pkg/configuration"
	prom "github.com/commercetools/telefonistka/internal/pkg/prometheus"
	"github.com/google/go-github/v62/github"
	log "github.com/sirupsen/logrus"
	yaml "gopkg.in/yaml.v2"
)

type PromotionInstance struct {
	Metadata          PromotionInstanceMetaData `deep:"-"` // Unit tests ignore Metadata currently
	ComputedSyncPaths map[string]string         // key is target, value is source
}

type PromotionInstanceMetaData struct {
	SourcePath                     string
	TargetPaths                    []string
	TargetDescription              string
	PerComponentSkippedTargetPaths map[string][]string // ComponentName is the key,
	ComponentNames                 []string
	AutoMerge                      bool
	Labels                         []string
}

func containMatchingRegex(patterns []string, str string) bool {
	for _, pattern := range patterns {
		doesElementMatchPattern, err := regexp.MatchString(pattern, str)
		if err != nil {
			log.Errorf("failed to match regex %s vs %s\n%s", pattern, str, err)
			return false
		}
		if doesElementMatchPattern {
			return true
		}
	}
	return false
}

func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}

func DetectDrift(ghPrClientDetails GhPrClientDetails) error {
	ghPrClientDetails.PrLogger.Debugln("Checking for Drift")
	if ghPrClientDetails.Ctx.Err() != nil {
		return ghPrClientDetails.Ctx.Err()
	}
	diffOutputMap := make(map[string]string)
	defaultBranch, _ := ghPrClientDetails.GetDefaultBranch()
	config, err := GetInRepoConfig(ghPrClientDetails, defaultBranch)
	if err != nil {
		_ = ghPrClientDetails.CommentOnPr(fmt.Sprintf("Failed to get configuration\n```\n%s\n```\n", err))
		return err
	}

	promotions, _ := GeneratePromotionPlan(ghPrClientDetails, config, ghPrClientDetails.Ref)

	for _, promotion := range promotions {
		ghPrClientDetails.PrLogger.Debugf("Checking drift for %s", promotion.Metadata.SourcePath)
		for trgt, src := range promotion.ComputedSyncPaths {
			hasDiff, diffOutput, _ := CompareRepoDirectories(ghPrClientDetails, src, trgt, defaultBranch)
			if hasDiff {
				mapKey := fmt.Sprintf("`%s` ↔️  `%s`", src, trgt)
				diffOutputMap[mapKey] = diffOutput
				ghPrClientDetails.PrLogger.Debugf("Found diff @ %s", mapKey)
			}
		}
	}
	if len(diffOutputMap) != 0 {
		templateOutput, err := executeTemplate("driftMsg", defaultTemplatesFullPath("drift-pr-comment.gotmpl"), diffOutputMap)
		if err != nil {
			return err
		}

		err = commentPR(ghPrClientDetails, templateOutput)
		if err != nil {
			return err
		}
	} else {
		ghPrClientDetails.PrLogger.Infof("No drift found")
	}

	return nil
}

func getComponentConfig(ghPrClientDetails GhPrClientDetails, componentPath string, branch string) (*cfg.ComponentConfig, error) {
	componentConfig := &cfg.ComponentConfig{}
	rGetContentOps := &github.RepositoryContentGetOptions{Ref: branch}
	ghPrClientDetails.PrLogger.Debugf("Loading component-level config from: %s/telefonistka.yaml (branch: %s)", componentPath, branch)
	componentConfigFileContent, _, resp, err := ghPrClientDetails.GhClientPair.v3Client.Repositories.GetContents(ghPrClientDetails.Ctx, ghPrClientDetails.Owner, ghPrClientDetails.Repo, componentPath+"/telefonistka.yaml", rGetContentOps)
	prom.InstrumentGhCall(resp)
	if (err != nil) && (resp.StatusCode != 404) { // The file is optional
		ghPrClientDetails.PrLogger.Errorf("could not get file list from GH API: err=%s\nresponse=%v", err, resp)
		return nil, err
	} else if resp.StatusCode == 404 {
		ghPrClientDetails.PrLogger.Debugf("No in-component config found in %s (404)", componentPath)
		return &cfg.ComponentConfig{}, nil
	}
	componentConfigFileContentString, _ := componentConfigFileContent.GetContent()
	err = yaml.Unmarshal([]byte(componentConfigFileContentString), componentConfig)
	if err != nil {
		ghPrClientDetails.PrLogger.Errorf("Failed to parse component configuration for %s: err=%s\n", componentPath, err) // TODO comment this error to PR
		return nil, err
	}
	if len(componentConfig.PromotionTargetBlockList) > 0 || len(componentConfig.PromotionTargetAllowList) > 0 {
		ghPrClientDetails.PrLogger.Infof("Loaded component config for %s: blockList=%v, allowList=%v, disableArgoCDDiff=%v", componentPath, componentConfig.PromotionTargetBlockList, componentConfig.PromotionTargetAllowList, componentConfig.DisableArgoCDDiff)
	} else {
		ghPrClientDetails.PrLogger.Debugf("Loaded component config for %s (no block/allow lists)", componentPath)
	}
	return componentConfig, nil
}

// This function generates a list of "components" that where changed in the PR and are relevant for promotion)
func generateListOfRelevantComponents(ghPrClientDetails GhPrClientDetails, config *cfg.Config) (relevantComponents map[relevantComponent]struct{}, err error) {
	relevantComponents = make(map[relevantComponent]struct{})

	// Get the list of files in the PR, with pagination
	opts := &github.ListOptions{}
	prFiles := []*github.CommitFile{}

	for {
		perPagePrFiles, resp, err := ghPrClientDetails.GhClientPair.v3Client.PullRequests.ListFiles(ghPrClientDetails.Ctx, ghPrClientDetails.Owner, ghPrClientDetails.Repo, ghPrClientDetails.PrNumber, opts)
		prom.InstrumentGhCall(resp)
		if err != nil {
			ghPrClientDetails.PrLogger.Errorf("could not get file list from GH API: err=%s\nstatus code=%v", err, resp.Response.Status)
			return nil, err
		}
		prFiles = append(prFiles, perPagePrFiles...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	for _, changedFile := range prFiles {
		for _, promotionPathConfig := range config.PromotionPaths {
			if match, _ := regexp.MatchString("^"+promotionPathConfig.SourcePath+".*", *changedFile.Filename); match {
				// "components" here are the sub directories of the SourcePath
				// but with promotionPathConfig.ComponentPathExtraDepth we can grab multiple levels of subdirectories,
				// to support cases where components are nested deeper(e.g. [SourcePath]/owningTeam/namespace/component1)
				componentPathRegexSubSstrings := []string{}
				for i := 0; i <= promotionPathConfig.ComponentPathExtraDepth; i++ {
					componentPathRegexSubSstrings = append(componentPathRegexSubSstrings, "[^/]*")
				}
				componentPathRegexSubString := strings.Join(componentPathRegexSubSstrings, "/")
				getComponentRegexString := regexp.MustCompile("^" + promotionPathConfig.SourcePath + "(" + componentPathRegexSubString + ")/.*")
				componentName := getComponentRegexString.ReplaceAllString(*changedFile.Filename, "${1}")

				getSourcePathRegexString := regexp.MustCompile("^(" + promotionPathConfig.SourcePath + ")" + componentName + "/.*")
				compiledSourcePath := getSourcePathRegexString.ReplaceAllString(*changedFile.Filename, "${1}")
				relevantComponentsElement := relevantComponent{
					SourcePath:    compiledSourcePath,
					ComponentName: componentName,
					AutoMerge:     promotionPathConfig.Conditions.AutoMerge,
				}
				relevantComponents[relevantComponentsElement] = struct{}{}
				break // a file can only be a single "source dir"
			}
		}
	}
	return relevantComponents, nil
}

type relevantComponent struct {
	SourcePath    string
	ComponentName string
	AutoMerge     bool
}

func generateListOfChangedComponentPaths(ghPrClientDetails GhPrClientDetails, config *cfg.Config) (changedComponentPaths []string, err error) {
	// If the PR has a list of promoted paths in the PR Telefonistika metadata(=is a promotion PR), we use that
	if len(ghPrClientDetails.PrMetadata.PromotedPaths) > 0 {
		changedComponentPaths = ghPrClientDetails.PrMetadata.PromotedPaths
		return changedComponentPaths, nil
	}

	// If not we will use in-repo config to generate it, and turns the map with struct keys into a list of strings
	relevantComponents, err := generateListOfRelevantComponents(ghPrClientDetails, config)
	if err != nil {
		return nil, err
	}
	for component := range relevantComponents {
		changedComponentPaths = append(changedComponentPaths, component.SourcePath+component.ComponentName)
	}
	return changedComponentPaths, nil
}

// This function generates a promotion plan based on the list of relevant components that where "touched" and the in-repo telefonitka  configuration
func generatePlanBasedOnChangeddComponent(ghPrClientDetails GhPrClientDetails, config *cfg.Config, relevantComponents map[relevantComponent]struct{}, configBranch string) (promotions map[string]PromotionInstance, err error) {
	ghPrClientDetails.PrLogger.Infof("Starting promotion plan generation for %d relevant components", len(relevantComponents))
	promotions = make(map[string]PromotionInstance)
	for componentToPromote := range relevantComponents {
		ghPrClientDetails.PrLogger.Debugf("Processing component: sourcePath=%s, componentName=%s, autoMerge=%v", componentToPromote.SourcePath, componentToPromote.ComponentName, componentToPromote.AutoMerge)
		componentConfig, err := getComponentConfig(ghPrClientDetails, componentToPromote.SourcePath+componentToPromote.ComponentName, configBranch)
		if err != nil {
			ghPrClientDetails.PrLogger.Errorf("Failed to get in component configuration, err=%s\nskipping %s", err, componentToPromote.SourcePath+componentToPromote.ComponentName)
			continue
		}

		for _, configPromotionPath := range config.PromotionPaths {
			if match, _ := regexp.MatchString(configPromotionPath.SourcePath, componentToPromote.SourcePath); match {
				ghPrClientDetails.PrLogger.Debugf("Component %s matches promotionPath sourcePath: %s", componentToPromote.ComponentName, configPromotionPath.SourcePath)
				// This section checks if a PromotionPath has a condition and skips it if needed
				if configPromotionPath.Conditions.PrHasLabels != nil {
					ghPrClientDetails.PrLogger.Debugf("PromotionPath has label conditions: %v", configPromotionPath.Conditions.PrHasLabels)
					thisPrHasTheRightLabel := false
					for _, l := range ghPrClientDetails.Labels {
						if contains(configPromotionPath.Conditions.PrHasLabels, *l.Name) {
							ghPrClientDetails.PrLogger.Debugf("PR label '%s' matches condition", *l.Name)
							thisPrHasTheRightLabel = true
							break
						}
					}
					if !thisPrHasTheRightLabel {
						ghPrClientDetails.PrLogger.Debugf("PR labels don't match condition, skipping promotionPath: %s", configPromotionPath.SourcePath)
						continue
					}
				}

				for _, ppr := range configPromotionPath.PromotionPrs {
					sort.Strings(ppr.TargetPaths)
					ghPrClientDetails.PrLogger.Debugf("Processing PromotionPr with targetPaths: %v, targetDescription: %s", ppr.TargetPaths, ppr.TargetDescription)

					mapKey := configPromotionPath.SourcePath + ">" + strings.Join(ppr.TargetPaths, "|") // This key is used to aggregate the PR based on source and target combination
					if entry, ok := promotions[mapKey]; !ok {
						ghPrClientDetails.PrLogger.Infof("Creating new promotion: %s", mapKey)
						if ppr.TargetDescription == "" {
							ppr.TargetDescription = strings.Join(ppr.TargetPaths, " ")
						}
						promotions[mapKey] = PromotionInstance{
							Metadata: PromotionInstanceMetaData{
								TargetPaths:                    ppr.TargetPaths,
								TargetDescription:              ppr.TargetDescription,
								SourcePath:                     componentToPromote.SourcePath,
								ComponentNames:                 []string{componentToPromote.ComponentName},
								PerComponentSkippedTargetPaths: map[string][]string{},
								AutoMerge:                      componentToPromote.AutoMerge,
								Labels:                         ppr.Labels,
							},
							ComputedSyncPaths: map[string]string{},
						}
					} else if !contains(entry.Metadata.ComponentNames, componentToPromote.ComponentName) {
						entry.Metadata.ComponentNames = append(entry.Metadata.ComponentNames, componentToPromote.ComponentName)
						promotions[mapKey] = entry
					}

					for _, indevidualPath := range ppr.TargetPaths {
						if componentConfig != nil {
							// BlockList supersedes Allowlist, if something matched there the entry is ignored regardless of allowlist
							if componentConfig.PromotionTargetBlockList != nil {
								ghPrClientDetails.PrLogger.Debugf("Checking target '%s' against blockList: %v", indevidualPath, componentConfig.PromotionTargetBlockList)
								if containMatchingRegex(componentConfig.PromotionTargetBlockList, indevidualPath) {
									ghPrClientDetails.PrLogger.Warnf("Target '%s' blocked by blockList pattern for component %s", indevidualPath, componentToPromote.ComponentName)
									promotions[mapKey].Metadata.PerComponentSkippedTargetPaths[componentToPromote.ComponentName] = append(promotions[mapKey].Metadata.PerComponentSkippedTargetPaths[componentToPromote.ComponentName], indevidualPath)
									continue
								}
							}
							if componentConfig.PromotionTargetAllowList != nil {
								ghPrClientDetails.PrLogger.Debugf("Checking target '%s' against allowList: %v", indevidualPath, componentConfig.PromotionTargetAllowList)
								if !containMatchingRegex(componentConfig.PromotionTargetAllowList, indevidualPath) {
									ghPrClientDetails.PrLogger.Warnf("Target '%s' not allowed by allowList pattern for component %s", indevidualPath, componentToPromote.ComponentName)
									promotions[mapKey].Metadata.PerComponentSkippedTargetPaths[componentToPromote.ComponentName] = append(promotions[mapKey].Metadata.PerComponentSkippedTargetPaths[componentToPromote.ComponentName], indevidualPath)
									continue
								}
							}
						}
						ghPrClientDetails.PrLogger.Debugf("Adding sync path for promotion: target=%s, source=%s", indevidualPath+componentToPromote.ComponentName, componentToPromote.SourcePath+componentToPromote.ComponentName)
						promotions[mapKey].ComputedSyncPaths[indevidualPath+componentToPromote.ComponentName] = componentToPromote.SourcePath + componentToPromote.ComponentName
					}
				}
				break
			}
		}
	}
	ghPrClientDetails.PrLogger.Infof("Promotion plan generation complete: %d promotion instances created", len(promotions))
	for key, promotion := range promotions {
		ghPrClientDetails.PrLogger.Debugf("Promotion %s: %d sync paths, %d components", key, len(promotion.ComputedSyncPaths), len(promotion.Metadata.ComponentNames))
	}
	return promotions, nil
}

func GeneratePromotionPlan(ghPrClientDetails GhPrClientDetails, config *cfg.Config, configBranch string) (map[string]PromotionInstance, error) {
	// TODO refactor tests to use the two functions below instead of this one
	relevantComponents, err := generateListOfRelevantComponents(ghPrClientDetails, config)
	if err != nil {
		return nil, err
	}
	promotions, err := generatePlanBasedOnChangeddComponent(ghPrClientDetails, config, relevantComponents, configBranch)
	return promotions, err
}
