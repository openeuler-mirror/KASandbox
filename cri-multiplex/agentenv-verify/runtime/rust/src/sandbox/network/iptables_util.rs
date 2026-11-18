// agentenv-verify: insert-tests
    #[derive(Clone, Debug, Eq, PartialEq)]
    struct FakeTables(std::collections::HashMap<(&'static str, &'static str), Vec<String>>);

    impl FakeTables {
        fn apply(&mut self, command: &IptablesRestoreCommand) -> Result<()> {
            match command {
                IptablesRestoreCommand::NewChain { table, chain } => {
                    if self.0.contains_key(&(*table, *chain)) {
                        return Err(anyhow!("chain exists"));
                    }
                    self.0.insert((*table, *chain), Vec::new());
                }
                IptablesRestoreCommand::FlushChain { table, chain } => {
                    self.0
                        .get_mut(&(*table, *chain))
                        .context("missing chain")?
                        .clear();
                }
                IptablesRestoreCommand::DeleteChain { table, chain } => {
                    if !self
                        .0
                        .get(&(*table, *chain))
                        .context("missing chain")?
                        .is_empty()
                    {
                        return Err(anyhow!("chain not empty"));
                    }
                    self.0.remove(&(*table, *chain));
                }
                IptablesRestoreCommand::Append { table, chain, rule }
                | IptablesRestoreCommand::Insert {
                    table, chain, rule, ..
                } => {
                    self.0
                        .get_mut(&(*table, *chain))
                        .context("missing chain")?
                        .push(rule.clone());
                }
                IptablesRestoreCommand::Delete { table, chain, rule } => {
                    let rules = self.0.get_mut(&(*table, *chain)).context("missing chain")?;
                    let index = rules
                        .iter()
                        .position(|item| item == rule)
                        .context("missing rule")?;
                    rules.remove(index);
                }
            }
            Ok(())
        }
    }


    #[test]
    fn p1_partial_external_rules_rollback_only_successful_owned_resources() {
        let commands = vec![
            IptablesRestoreCommand::Append {
                table: "filter",
                chain: "FORWARD",
                rule: "-i tap0 -j ACCEPT".into(),
            },
            IptablesRestoreCommand::NewChain {
                table: "filter",
                chain: "OWNED",
            },
            IptablesRestoreCommand::FlushChain {
                table: "filter",
                chain: "OWNED",
            },
            IptablesRestoreCommand::Append {
                table: "filter",
                chain: "OWNED",
                rule: "-j ACCEPT".into(),
            },
            IptablesRestoreCommand::NewChain {
                table: "nat",
                chain: "OWNED-NAT",
            },
            IptablesRestoreCommand::Insert {
                table: "nat",
                chain: "PREROUTING",
                position: 1,
                rule: "-i tap0 -j OWNED-NAT".into(),
            },
        ];
        let initial = FakeTables(std::collections::HashMap::from([
            (("filter", "FORWARD"), vec!["-i tap0 -j ACCEPT".to_string()]),
            (("nat", "PREROUTING"), vec!["-i eth0 -j ACCEPT".to_string()]),
        ]));
        // 在每个步骤之前用桩返回错误，覆盖未创建、部分创建及全部完成；不调用 iptables。
        for failure_at in 0..=commands.len() {
            let mut world = initial.clone();
            let mut undo = Vec::new();
            let mut index = 0;
            let result =
                apply_owned_iptables_commands_with(&commands, "aenv-test", &mut undo, |command| {
                    if index == failure_at {
                        return Err(anyhow!("simulated command failure"));
                    }
                    index += 1;
                    world.apply(command)
                });
            assert_eq!(result.is_ok(), failure_at == commands.len());
            while let Some(command) = undo.pop() {
                world.apply(&command).unwrap();
            }
            assert_eq!(world, initial, "failure at {failure_at}");
        }
    }



    #[test]
    fn p1_existing_external_chain_never_becomes_owned() {
        let initial = FakeTables(std::collections::HashMap::from([(
            ("filter", "FOREIGN"),
            vec!["-j DROP".to_string()],
        )]));
        let mut world = initial.clone();
        let mut undo = Vec::new();
        let commands = [
            IptablesRestoreCommand::NewChain {
                table: "filter",
                chain: "FOREIGN",
            },
            IptablesRestoreCommand::FlushChain {
                table: "filter",
                chain: "FOREIGN",
            },
        ];
        assert!(
            apply_owned_iptables_commands_with(&commands, "aenv-test", &mut undo, |command| world
                .apply(command))
            .is_err()
        );
        assert!(undo.is_empty());
        assert_eq!(world, initial);
        assert!(
            apply_owned_iptables_commands_with(&commands[1..], "aenv-test", &mut undo, |command| {
                world.apply(command)
            })
            .is_err()
        );
        assert_eq!(world, initial);
    }


    #[test]
    fn build_restore_script_renders_delete_chain() {
        let script = build_restore_script(&[
            IptablesRestoreCommand::FlushChain {
                table: "filter",
                chain: "AGENTENV-USER-EGRESS",
            },
            IptablesRestoreCommand::DeleteChain {
                table: "filter",
                chain: "AGENTENV-USER-EGRESS",
            },
        ]);

        assert_eq!(
            script,
            "*filter\n-F AGENTENV-USER-EGRESS\n-X AGENTENV-USER-EGRESS\nCOMMIT\n"
        );
    }
