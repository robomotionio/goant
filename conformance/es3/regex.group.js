function check(a,b){if(a!==b)throw a+" !== "+b;} check('foobar'.match(/(foo)(bar)/)[1],'foo');check('foobar'.match(/(foo)(bar)/)[2],'bar');console.log('PASS');
